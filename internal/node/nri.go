package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/anchor-dra/anchor/internal/constants"
	anchorkube "github.com/anchor-dra/anchor/internal/kube"
	anchormetrics "github.com/anchor-dra/anchor/internal/metrics"
	"github.com/anchor-dra/anchor/internal/model"
)

type NRIPlugin struct {
	core      kubernetes.Interface
	dynamic   dynamic.Interface
	store     *PlanStore
	injector  LinkInjector
	logger    *slog.Logger
	healthy   atomic.Bool
	connected atomic.Bool
	networkMu sync.Mutex
}

var errPlanStore = errors.New("pod network plan store failure")

var runPodSandboxMinimumVersion = utilversion.MustParseGeneric("2.2.6")

func NewNRIPlugin(core kubernetes.Interface, dynamicClient dynamic.Interface, store *PlanStore, injector LinkInjector, logger *slog.Logger) *NRIPlugin {
	return &NRIPlugin{core: core, dynamic: dynamicClient, store: store, injector: injector, logger: logger}
}

func (p *NRIPlugin) Healthy() bool { return p.healthy.Load() }

// Configure selects the earliest safe NRI lifecycle event exposed by the
// connected runtime. containerd versions before 2.2.6 can leak a partially
// created sandbox when a RunPodSandbox hook fails, so those versions inject at
// CreateContainer instead. On fixed versions CreateContainer remains an
// idempotent guard for sandboxes created while the plugin was absent. An
// unknown version takes the conservative path.
func (p *NRIPlugin) Configure(_ context.Context, _ string, runtimeName, runtimeVersion string) (api.EventMask, error) {
	if !strings.EqualFold(strings.TrimSpace(runtimeName), "containerd") {
		return 0, fmt.Errorf("unsupported NRI runtime %q", runtimeName)
	}

	var events api.EventMask
	version, err := utilversion.ParseGeneric(runtimeVersion)
	if err != nil {
		events.Set(api.Event_CREATE_CONTAINER)
		p.logger.Warn("cannot parse containerd version; selecting conservative CreateContainer injection",
			"runtime", runtimeName, "version", runtimeVersion, "error", err)
		return events, nil
	}
	if version.AtLeast(runPodSandboxMinimumVersion) {
		events.Set(api.Event_RUN_POD_SANDBOX, api.Event_CREATE_CONTAINER)
		p.logger.Info("selected NRI injection lifecycle", "runtime", runtimeName, "version", runtimeVersion, "event", "RunPodSandbox", "lateRegistrationGuard", "CreateContainer")
		return events, nil
	}

	events.Set(api.Event_CREATE_CONTAINER)
	p.logger.Info("selected NRI injection lifecycle", "runtime", runtimeName, "version", runtimeVersion, "event", "CreateContainer")
	return events, nil
}

func (p *NRIPlugin) Run(ctx context.Context, socketPath string) error {
	if socketPath == "" {
		socketPath = api.DefaultSocketPath
	}
	for ctx.Err() == nil {
		service, err := stub.New(p,
			stub.WithPluginName(constants.NRIPluginName),
			stub.WithPluginIdx("10"),
			stub.WithSocketPath(socketPath),
			stub.WithOnClose(func() { p.connected.Store(false); p.healthy.Store(false) }),
		)
		if err != nil {
			return fmt.Errorf("create NRI plugin: %w", err)
		}
		err = service.Run(ctx)
		p.connected.Store(false)
		p.healthy.Store(false)
		if ctx.Err() != nil {
			return nil
		}
		p.logger.Error("NRI connection lost; retrying", "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
	return nil
}

func (p *NRIPlugin) Synchronize(ctx context.Context, pods []*api.PodSandbox, _ []*api.Container) ([]*api.ContainerUpdate, error) {
	for _, pod := range pods {
		plans, err := p.store.ForPod(pod.GetUid())
		if err != nil {
			p.healthy.Store(false)
			return nil, err
		}
		if len(plans) == 0 {
			continue
		}
		namespace, err := sandboxNetworkNamespace(pod)
		if err != nil {
			var storeErrors []error
			for index := range plans {
				if persistErr := p.recordFailure(ctx, &plans[index], err); persistErr != nil {
					storeErrors = append(storeErrors, persistErr)
				}
			}
			if persistErr := errors.Join(storeErrors...); persistErr != nil {
				p.healthy.Store(false)
				return nil, persistErr
			}
			p.logger.Error("cannot reconcile pod network namespace", "pod", pod.GetNamespace()+"/"+pod.GetName(), "error", err)
			continue
		}
		if err := p.injectPlans(ctx, pod, plans, namespace); err != nil {
			if errors.Is(err, errPlanStore) {
				p.healthy.Store(false)
				return nil, err
			}
			p.logger.Error("pod network synchronization failed", "pod", pod.GetNamespace()+"/"+pod.GetName(), "error", err)
		}
	}
	if err := p.Reconcile(ctx); err != nil {
		p.healthy.Store(false)
		return nil, err
	}
	p.connected.Store(true)
	p.healthy.Store(true)
	return nil, nil
}

func (p *NRIPlugin) RunPodSandbox(ctx context.Context, sandbox *api.PodSandbox) error {
	return p.injectPodSandbox(ctx, sandbox)
}

func (p *NRIPlugin) CreateContainer(ctx context.Context, sandbox *api.PodSandbox, _ *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	err := p.injectPodSandbox(ctx, sandbox)
	return nil, nil, err
}

func (p *NRIPlugin) injectPodSandbox(ctx context.Context, sandbox *api.PodSandbox) error {
	if sandbox.GetUid() == "" {
		return errors.New("NRI sandbox has no Kubernetes pod UID")
	}
	plans, err := p.store.ForPod(sandbox.GetUid())
	if err != nil {
		return err
	}
	claimed, err := p.anchorClaimCount(ctx, sandbox)
	if err != nil {
		return err
	}
	if claimed == 0 && len(plans) == 0 {
		return nil
	}
	if claimed != len(plans) {
		return fmt.Errorf("pod %s/%s has %d allocated Anchor claims but %d persisted plans", sandbox.GetNamespace(), sandbox.GetName(), claimed, len(plans))
	}
	namespace, err := sandboxNetworkNamespace(sandbox)
	if err != nil {
		return err
	}
	return p.injectPlans(ctx, sandbox, plans, namespace)
}

func (p *NRIPlugin) injectPlans(ctx context.Context, sandbox *api.PodSandbox, plans []PodNetworkPlan, namespace NetworkNamespace) error {
	p.networkMu.Lock()
	defer p.networkMu.Unlock()
	for _, plan := range plans {
		if plan.PodNamespace != sandbox.GetNamespace() || plan.PodName != sandbox.GetName() || plan.PodUID != sandbox.GetUid() {
			return fmt.Errorf("persisted plan identity does not match sandbox %s/%s UID %s", sandbox.GetNamespace(), sandbox.GetName(), sandbox.GetUid())
		}
		if plan.Phase != PlanInjectionPending && plan.Phase != PlanInjected && plan.Phase != PlanRecovering {
			return fmt.Errorf("claim %s/%s network plan is not ready for injection (phase %s)", plan.ClaimNamespace, plan.ClaimName, plan.Phase)
		}
		previousPhase := plan.Phase
		previousNamespace := plan.NetworkNamespace
		plan.NetworkNamespace = namespace.Path
		if previousPhase == PlanInjected && previousNamespace == namespace.Path {
			if verifyErr := p.injector.Verify(ctx, plan, namespace); verifyErr == nil {
				continue
			} else {
				if injectErr := p.injector.Inject(ctx, plan, namespace); injectErr != nil {
					failure := errors.Join(verifyErr, injectErr)
					if persistErr := p.recordFailure(ctx, &plan, failure); persistErr != nil {
						return errors.Join(failure, persistErr)
					}
					return failure
				}
				if persistErr := p.recordFailure(ctx, &plan, verifyErr); persistErr != nil {
					return errors.Join(verifyErr, persistErr)
				}
				if err := p.recordSuccess(ctx, &plan, true); err != nil {
					return err
				}
				continue
			}
		}
		if err := p.injector.Inject(ctx, plan, namespace); err != nil {
			if persistErr := p.recordFailure(ctx, &plan, err); persistErr != nil {
				return errors.Join(err, persistErr)
			}
			return err
		}
		recovered := previousPhase == PlanRecovering || (previousPhase == PlanInjected && previousNamespace != namespace.Path)
		if err := p.recordSuccess(ctx, &plan, recovered); err != nil {
			return err
		}
	}
	return nil
}

// Reconcile verifies and repairs all live network plans. Per-workload errors
// are recorded and isolated; only an inability to inspect the plan store is a
// plugin-level health failure.
func (p *NRIPlugin) Reconcile(ctx context.Context) error {
	p.networkMu.Lock()
	defer p.networkMu.Unlock()
	results, err := p.injector.Reconcile(ctx)
	if err != nil {
		p.healthy.Store(false)
		return err
	}
	var storeErrors []error
	for index := range results {
		result := &results[index]
		if result.Plan.ClaimUID == "" {
			p.logger.Error("skipping unreadable persisted network plan", "error", result.Err)
			continue
		}
		if result.Err != nil {
			failure := result.Err
			if result.DriftErr != nil {
				failure = errors.Join(result.DriftErr, result.Err)
			}
			if err := p.recordFailure(ctx, &result.Plan, failure); err != nil {
				storeErrors = append(storeErrors, err)
			}
			continue
		}
		if result.Drifted || result.Plan.Phase == PlanRecovering {
			if result.DriftErr != nil {
				if err := p.recordFailure(ctx, &result.Plan, result.DriftErr); err != nil {
					storeErrors = append(storeErrors, err)
					continue
				}
			}
			if err := p.recordSuccess(ctx, &result.Plan, true); err != nil {
				p.logger.Error("persist repaired network plan", "claim", result.Plan.ClaimNamespace+"/"+result.Plan.ClaimName, "error", err)
				storeErrors = append(storeErrors, err)
			}
		}
	}
	if err := errors.Join(storeErrors...); err != nil {
		p.healthy.Store(false)
		return err
	}
	p.refreshFailureAge()
	if p.connected.Load() {
		p.healthy.Store(true)
	}
	return nil
}

func (p *NRIPlugin) recordSuccess(ctx context.Context, plan *PodNetworkPlan, recovered bool) error {
	plan.Phase = PlanInjected
	plan.LastError = ""
	plan.FirstFailureAt = nil
	if err := p.store.Save(*plan); err != nil {
		return fmt.Errorf("%w: persist successful injection for claim %s: %v", errPlanStore, plan.ClaimUID, err)
	}
	result := "success"
	if recovered {
		result = "recovered"
		p.emit(ctx, plan, corev1.EventTypeNormal, "NetworkInjectionRecovered", "Anchor repaired and verified the pod network namespace")
	}
	anchormetrics.Injections.WithLabelValues(result).Inc()
	_ = p.updateInjectionStatus(ctx, *plan, nil)
	p.refreshFailureAge()
	return nil
}

func (p *NRIPlugin) recordFailure(ctx context.Context, plan *PodNetworkPlan, injectionErr error) error {
	now := time.Now().UTC()
	plan.Phase = PlanRecovering
	plan.LastError = injectionErr.Error()
	plan.RetryCount++
	if plan.FirstFailureAt == nil {
		plan.FirstFailureAt = &now
	}
	if err := p.store.Save(*plan); err != nil {
		return fmt.Errorf("%w: persist failed injection for claim %s: %v", errPlanStore, plan.ClaimUID, err)
	}
	anchormetrics.Injections.WithLabelValues("error").Inc()
	anchormetrics.InjectionRetries.Inc()
	anchormetrics.InjectionFailureAge.Set(now.Sub(*plan.FirstFailureAt).Seconds())
	_ = p.updateInjectionStatus(ctx, *plan, injectionErr)
	p.emit(ctx, plan, corev1.EventTypeWarning, "NetworkInjectionFailed", injectionErr.Error())
	return nil
}

func (p *NRIPlugin) refreshFailureAge() {
	plans, _, err := p.store.LoadAllLenient()
	if err != nil {
		return
	}
	now := time.Now().UTC()
	oldest := 0.0
	for _, plan := range plans {
		if plan.Phase == PlanRecovering && plan.FirstFailureAt != nil {
			age := now.Sub(*plan.FirstFailureAt).Seconds()
			if age > oldest {
				oldest = age
			}
		}
	}
	anchormetrics.InjectionFailureAge.Set(oldest)
}

func (p *NRIPlugin) anchorClaimCount(ctx context.Context, sandbox *api.PodSandbox) (int, error) {
	pod, err := p.core.CoreV1().Pods(sandbox.GetNamespace()).Get(ctx, sandbox.GetName(), metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("get NRI pod from Kubernetes: %w", err)
	}
	if string(pod.UID) != sandbox.GetUid() {
		return 0, fmt.Errorf("NRI pod UID %s does not match Kubernetes UID %s", sandbox.GetUid(), pod.UID)
	}
	count := 0
	claimNames, err := podResourceClaimNames(pod)
	if err != nil {
		return 0, err
	}
	for _, claimName := range claimNames {
		claim, err := p.core.ResourceV1().ResourceClaims(pod.Namespace).Get(ctx, claimName, metav1.GetOptions{})
		if err != nil {
			return 0, fmt.Errorf("get ResourceClaim %s: %w", claimName, err)
		}
		if _, err := anchorkube.AllocationForDriver(claim); err == nil {
			count++
		} else if !errors.Is(err, anchorkube.ErrNoAllocationForDriver) {
			return 0, err
		}
	}
	return count, nil
}

func podResourceClaimNames(pod *corev1.Pod) ([]string, error) {
	generated := make(map[string]string, len(pod.Status.ResourceClaimStatuses))
	for _, status := range pod.Status.ResourceClaimStatuses {
		if status.ResourceClaimName != nil {
			generated[status.Name] = *status.ResourceClaimName
		}
	}
	names := make([]string, 0, len(pod.Spec.ResourceClaims))
	for _, reference := range pod.Spec.ResourceClaims {
		switch {
		case reference.ResourceClaimName != nil:
			names = append(names, *reference.ResourceClaimName)
		case reference.ResourceClaimTemplateName != nil:
			name := generated[reference.Name]
			if name == "" {
				return nil, fmt.Errorf("pod ResourceClaim %q has no generated claim name in status", reference.Name)
			}
			names = append(names, name)
		default:
			return nil, fmt.Errorf("pod ResourceClaim %q has no claim or template source", reference.Name)
		}
	}
	return names, nil
}

func sandboxNetworkNamespace(sandbox *api.PodSandbox) (NetworkNamespace, error) {
	if linux := sandbox.GetLinux(); linux != nil {
		for _, namespace := range linux.GetNamespaces() {
			if (namespace.GetType() == "network" || namespace.GetType() == "net") && namespace.GetPath() != "" {
				return NetworkNamespace{Path: namespace.GetPath()}, nil
			}
		}
	}
	if sandbox.GetPid() != 0 {
		path := fmt.Sprintf("/proc/%d/ns/net", sandbox.GetPid())
		if _, err := os.Stat(path); err == nil {
			return NetworkNamespace{Path: path}, nil
		}
	}
	return NetworkNamespace{}, errors.New("NRI sandbox did not expose a usable network namespace")
}

func (p *NRIPlugin) updateInjectionStatus(ctx context.Context, plan PodNetworkPlan, injectionErr error) error {
	if p.dynamic == nil {
		return nil
	}
	resource := p.dynamic.Resource(anchorkube.PlacementGVR).Namespace(plan.ClaimNamespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		object, err := resource.Get(ctx, plan.PlacementName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		status := model.EndpointPlacementStatus{}
		raw, _, _ := unstructured.NestedMap(object.Object, "status")
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &status); err != nil {
			return err
		}
		now := metav1.Now()
		injection := &model.InjectionStatus{Phase: string(plan.Phase), LastAttemptAt: &now, RetryCount: plan.RetryCount}
		if plan.FirstFailureAt != nil {
			first := metav1.NewTime(*plan.FirstFailureAt)
			injection.FirstFailureAt = &first
		}
		if injectionErr != nil {
			injection.LastError = injectionErr.Error()
		}
		status.Injection = injection
		encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
		if err != nil {
			return err
		}
		copy := object.DeepCopy()
		copy.Object["status"] = encoded
		_, err = resource.UpdateStatus(ctx, copy, metav1.UpdateOptions{})
		return err
	})
}

func (p *NRIPlugin) emit(ctx context.Context, plan *PodNetworkPlan, eventType, reason, message string) {
	if p.core == nil {
		return
	}
	now := metav1.Now()
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{GenerateName: "anchor-", Namespace: plan.ClaimNamespace},
		InvolvedObject: corev1.ObjectReference{APIVersion: "resource.k8s.io/v1", Kind: "ResourceClaim", Namespace: plan.ClaimNamespace, Name: plan.ClaimName, UID: types.UID(plan.ClaimUID)},
		Type:           eventType, Reason: reason, Message: message, FirstTimestamp: now, LastTimestamp: now, Count: 1, Source: corev1.EventSource{Component: "anchor-node"},
	}
	_, _ = p.core.CoreV1().Events(plan.ClaimNamespace).Create(ctx, event, metav1.CreateOptions{})
}

var _ stub.SynchronizeInterface = (*NRIPlugin)(nil)
var _ stub.ConfigureInterface = (*NRIPlugin)(nil)
var _ stub.RunPodInterface = (*NRIPlugin)(nil)
var _ stub.CreateContainerInterface = (*NRIPlugin)(nil)
