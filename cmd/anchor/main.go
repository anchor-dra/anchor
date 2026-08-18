package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	anchoraws "github.com/anchor-dra/anchor/internal/aws"
	"github.com/anchor-dra/anchor/internal/constants"
	"github.com/anchor-dra/anchor/internal/controller"
	anchorkube "github.com/anchor-dra/anchor/internal/kube"
	"github.com/anchor-dra/anchor/internal/node"
	"github.com/anchor-dra/anchor/internal/version"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	if err := command(logger).ExecuteContext(ctx); err != nil {
		logger.Error("anchor stopped", "error", err)
		os.Exit(1)
	}
}

func command(logger *slog.Logger) *cobra.Command {
	root := &cobra.Command{Use: "anchor", Short: "AWS static endpoint DRA driver", SilenceUsage: true}
	root.Version = version.Version
	root.AddCommand(controllerCommand(logger), nodeCommand(logger))
	return root
}

func controllerCommand(logger *slog.Logger) *cobra.Command {
	var kubeconfig, region, nodeLabel, namespace, identity, metricsAddress string
	var interval, inventoryInterval, driftInterval time.Duration
	var qps float64
	var burst int
	cmd := &cobra.Command{
		Use: "controller", Short: "run the central AWS placement controller",
		RunE: func(cmd *cobra.Command, _ []string) error {
			clients, err := anchorkube.NewClients(kubeconfig)
			if err != nil {
				return err
			}
			awsCfg, err := awsconfig.LoadDefaultConfig(cmd.Context(), awsconfig.WithRegion(region), awsconfig.WithRetryMaxAttempts(8))
			if err != nil {
				return fmt.Errorf("load AWS configuration: %w", err)
			}
			ec2Client := awsec2.NewFromConfig(awsCfg)
			limitedEC2 := anchoraws.NewRateLimitedEC2(ec2Client, qps, burst)
			strategy := anchoraws.NewIPReassign(limitedEC2, logger)
			reconciler := &controller.Controller{
				Inventory: &controller.InventoryReconciler{Core: clients.Core, Dynamic: clients.Dynamic, EC2: limitedEC2, NodeLabel: nodeLabel, Logger: logger},
				Placement: &controller.PlacementReconciler{Core: clients.Core, Dynamic: clients.Dynamic, Strategy: strategy, Logger: logger},
				Interval:  interval, InventoryInterval: inventoryInterval, DriftInterval: driftInterval, Logger: logger,
			}
			server := startMetrics(cmd.Context(), metricsAddress, logger)
			defer server.Shutdown(context.Background()) //nolint:errcheck
			if identity == "" {
				host, _ := os.Hostname()
				identity = host
			}
			lock := &resourcelock.LeaseLock{
				LeaseMeta:  metav1.ObjectMeta{Name: "anchor-controller", Namespace: namespace},
				Client:     clients.Core.CoordinationV1(),
				LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
			}
			var runErr error
			leaderelection.RunOrDie(cmd.Context(), leaderelection.LeaderElectionConfig{
				Lock: lock, LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second, ReleaseOnCancel: true,
				Callbacks: leaderelection.LeaderCallbacks{
					OnStartedLeading: func(ctx context.Context) { runErr = reconciler.Run(ctx) },
					OnStoppedLeading: func() { logger.Info("leader election lost") },
				},
			})
			return runErr
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig path; empty uses in-cluster configuration")
	cmd.Flags().StringVar(&region, "aws-region", "", "AWS region")
	cmd.Flags().StringVar(&nodeLabel, "node-label", constants.DriverName+"/enabled=true", "label selector for eligible nodes")
	cmd.Flags().StringVar(&namespace, "namespace", constants.SystemNamespace, "leader-election namespace")
	cmd.Flags().StringVar(&identity, "leader-election-identity", os.Getenv("POD_NAME"), "leader-election identity")
	cmd.Flags().StringVar(&metricsAddress, "metrics-address", ":8080", "metrics and health listen address")
	cmd.Flags().DurationVar(&interval, "reconcile-interval", 2*time.Second, "pending placement reconciliation interval")
	cmd.Flags().DurationVar(&inventoryInterval, "inventory-interval", time.Minute, "AWS node inventory refresh interval")
	cmd.Flags().DurationVar(&driftInterval, "drift-interval", time.Minute, "Ready endpoint verification interval")
	cmd.Flags().Float64Var(&qps, "aws-api-qps", 2, "global EC2 API requests per second")
	cmd.Flags().IntVar(&burst, "aws-api-burst", 2, "global EC2 API request burst")
	_ = cmd.MarkFlagRequired("aws-region")
	return cmd
}

func nodeCommand(logger *slog.Logger) *cobra.Command {
	var kubeconfig string
	var config node.Config
	cmd := &cobra.Command{
		Use: "node", Short: "run the kubelet DRA node plugin",
		RunE: func(cmd *cobra.Command, _ []string) error {
			clients, err := anchorkube.NewClients(kubeconfig)
			if err != nil {
				return err
			}
			return node.Run(cmd.Context(), config, clients.Core, clients.Dynamic, logger)
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig path; empty uses in-cluster configuration")
	cmd.Flags().StringVar(&config.NodeName, "node-name", os.Getenv("NODE_NAME"), "Kubernetes node name")
	cmd.Flags().StringVar(&config.PodUID, "pod-uid", os.Getenv("POD_UID"), "node-plugin pod UID for rolling updates")
	cmd.Flags().StringVar(&config.RegistrarDir, "kubelet-registrar-directory", "/var/lib/kubelet/plugins_registry", "kubelet plugin registry")
	cmd.Flags().StringVar(&config.PluginsDir, "kubelet-plugins-directory", "/var/lib/kubelet/plugins", "kubelet plugins directory")
	cmd.Flags().DurationVar(&config.PreparationTimeout, "preparation-timeout", 45*time.Second, "maximum wait for AWS placement")
	cmd.Flags().DurationVar(&config.RefreshInterval, "refresh-interval", 2*time.Second, "node inventory refresh interval")
	_ = cmd.MarkFlagRequired("node-name")
	return cmd
}

func startMetrics(ctx context.Context, address string, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	return server
}
