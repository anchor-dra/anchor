package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"example.com/anchor/internal/constants"
	anchorkube "example.com/anchor/internal/kube"
	"example.com/anchor/internal/model"
)

type Config struct {
	NodeName           string
	PodUID             string
	RegistrarDir       string
	PluginsDir         string
	PreparationTimeout time.Duration
	RefreshInterval    time.Duration
}

type Plugin struct {
	config  Config
	core    kubernetes.Interface
	dynamic dynamic.Interface
	helper  *kubeletplugin.Helper
	logger  *slog.Logger
}

func Run(ctx context.Context, config Config, core kubernetes.Interface, dynamicClient dynamic.Interface, logger *slog.Logger) error {
	if config.NodeName == "" {
		return fmt.Errorf("node name is required")
	}
	if config.RegistrarDir == "" {
		config.RegistrarDir = kubeletplugin.KubeletRegistryDir
	}
	if config.PluginsDir == "" {
		config.PluginsDir = kubeletplugin.KubeletPluginsDir
	}
	if config.PreparationTimeout <= 0 {
		config.PreparationTimeout = 45 * time.Second
	}
	if config.RefreshInterval <= 0 {
		config.RefreshInterval = 2 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	pluginDataDir := filepath.Join(config.PluginsDir, constants.DriverName)
	if err := os.MkdirAll(pluginDataDir, 0o750); err != nil {
		return fmt.Errorf("create DRA plugin data directory: %w", err)
	}
	plugin := &Plugin{config: config, core: core, dynamic: dynamicClient, logger: logger}
	helper, err := kubeletplugin.Start(ctx, plugin,
		kubeletplugin.KubeClient(core),
		kubeletplugin.NodeName(config.NodeName),
		kubeletplugin.DriverName(constants.DriverName),
		kubeletplugin.RegistrarDirectoryPath(config.RegistrarDir),
		kubeletplugin.PluginDataDirectoryPath(pluginDataDir),
		kubeletplugin.RollingUpdate(types.UID(config.PodUID)),
	)
	if err != nil {
		return fmt.Errorf("start DRA kubelet plugin: %w", err)
	}
	plugin.helper = helper
	defer helper.Stop()

	if err := plugin.refreshInventory(ctx); err != nil {
		logger.Warn("initial inventory is not ready", "error", err)
		_ = helper.PublishResources(ctx, emptyResources(config.NodeName))
	}
	ticker := time.NewTicker(config.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := plugin.refreshInventory(ctx); err != nil {
				logger.Warn("inventory refresh failed", "error", err)
				_ = helper.PublishResources(ctx, emptyResources(config.NodeName))
			}
		}
	}
}

func (p *Plugin) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	results := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		results[claim.UID] = p.prepare(ctx, claim)
	}
	return results, nil
}

func (p *Plugin) prepare(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	class, params, err := anchorkube.AllocationParameters(claim)
	if err != nil {
		return kubeletplugin.PrepareResult{Err: err}
	}
	if len(claim.Status.ReservedFor) > 1 {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("claim %s/%s has %d consumers; anchor permits one", claim.Namespace, claim.Name, len(claim.Status.ReservedFor))}
	}
	allocation, err := allocationForDriver(claim)
	if err != nil {
		return kubeletplugin.PrepareResult{Err: err}
	}
	inventory, err := p.inventory(ctx)
	if err != nil {
		return kubeletplugin.PrepareResult{Err: err}
	}
	paths := inventory.Status.Interfaces[class.Profile]
	if len(paths) != len(class.Paths) {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("profile %q is not ready on node %s", class.Profile, p.config.NodeName)}
	}
	subnetCIDRs := map[string]string{}
	byName := map[string]model.ENIPath{}
	for _, path := range paths {
		subnetCIDRs[path.SubnetID] = path.SubnetCIDR
		byName[path.Name] = path
	}
	if err := params.Validate(class, subnetCIDRs); err != nil {
		return kubeletplugin.PrepareResult{Err: err}
	}
	placementPaths := make([]model.PlacementPath, 0, len(params.Addresses))
	for _, address := range params.Addresses {
		path, ok := byName[address.Path]
		if !ok || path.Interface == "" {
			return kubeletplugin.PrepareResult{Err: fmt.Errorf("path %q has no ready host interface", address.Path)}
		}
		placementPaths = append(placementPaths, model.PlacementPath{Name: path.Name, IP: address.IP, ENIID: path.ENIID, Interface: path.Interface, SubnetID: path.SubnetID, SubnetCIDR: path.SubnetCIDR})
	}
	force := strings.EqualFold(claim.Annotations[constants.ForceStealAnnotation], "true")
	spec := model.EndpointPlacementSpec{
		ClaimName: claim.Name, ClaimUID: string(claim.UID), NodeName: p.config.NodeName,
		RequestName: allocation.Request, PoolName: allocation.Pool, DeviceName: allocation.Device,
		Strategy: class.Strategy, ForceSteal: force, Paths: placementPaths,
	}
	generation, err := p.upsertPlacement(ctx, claim.Namespace, "claim-"+string(claim.UID), spec)
	if err != nil {
		return kubeletplugin.PrepareResult{Err: err}
	}
	err = wait.PollUntilContextTimeout(ctx, time.Second, p.config.PreparationTimeout, true, func(ctx context.Context) (bool, error) {
		object, err := p.dynamic.Resource(anchorkube.PlacementGVR).Namespace(claim.Namespace).Get(ctx, "claim-"+string(claim.UID), metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		phase, _, _ := unstructured.NestedString(object.Object, "status", "phase")
		observed, _, _ := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
		message, _, _ := unstructured.NestedString(object.Object, "status", "message")
		if phase == model.PlacementFailed && observed == generation {
			p.logger.Warn("controller reported placement failure; waiting for retry", "claim", claim.Namespace+"/"+claim.Name, "message", message)
		}
		return phase == model.PlacementReady && observed == generation, nil
	})
	if err != nil {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("wait for endpoint placement: %w", err)}
	}
	return kubeletplugin.PrepareResult{Devices: []kubeletplugin.Device{{Requests: []string{allocation.Request}, PoolName: allocation.Pool, DeviceName: allocation.Device}}}
}

func (p *Plugin) UnprepareResourceClaims(_ context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	result := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		result[claim.UID] = nil
	}
	return result, nil
}

func (p *Plugin) HandleError(ctx context.Context, err error, message string) {
	klog.FromContext(ctx).Error(err, message)
}

func allocationForDriver(claim *resourceapi.ResourceClaim) (*resourceapi.DeviceRequestAllocationResult, error) {
	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim has no allocation")
	}
	var result *resourceapi.DeviceRequestAllocationResult
	for i := range claim.Status.Allocation.Devices.Results {
		allocation := &claim.Status.Allocation.Devices.Results[i]
		if allocation.Driver != constants.DriverName {
			continue
		}
		if result != nil {
			return nil, fmt.Errorf("anchor claim must allocate exactly one endpoint slot")
		}
		result = allocation
	}
	if result == nil {
		return nil, fmt.Errorf("claim has no allocation for %s", constants.DriverName)
	}
	return result, nil
}

func (p *Plugin) upsertPlacement(ctx context.Context, namespace, name string, spec model.EndpointPlacementSpec) (int64, error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	if err != nil {
		return 0, err
	}
	resource := p.dynamic.Resource(anchorkube.PlacementGVR).Namespace(namespace)
	current, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		old, _, _ := unstructured.NestedMap(current.Object, "spec")
		if reflect.DeepEqual(old, raw) {
			return current.GetGeneration(), nil
		}
		current.Object["spec"] = raw
		updated, err := resource.Update(ctx, current, metav1.UpdateOptions{})
		if err != nil {
			return 0, err
		}
		return updated.GetGeneration(), nil
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.APIGroup + "/" + constants.APIVersion, "kind": "EndpointPlacement",
		"metadata": map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{constants.DriverName + "/claim-uid": spec.ClaimUID}},
		"spec":     raw,
	}}
	created, err := resource.Create(ctx, object, metav1.CreateOptions{})
	if err != nil {
		return 0, err
	}
	return created.GetGeneration(), nil
}

type inventoryValue struct {
	Spec   model.AnchorNodeInventorySpec
	Status model.AnchorNodeInventoryStatus
}

func (p *Plugin) inventory(ctx context.Context) (*inventoryValue, error) {
	object, err := p.dynamic.Resource(anchorkube.InventoryGVR).Get(ctx, p.config.NodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get node inventory: %w", err)
	}
	result := &inventoryValue{}
	rawSpec, _, _ := unstructured.NestedMap(object.Object, "spec")
	rawStatus, _, _ := unstructured.NestedMap(object.Object, "status")
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawSpec, &result.Spec); err != nil {
		return nil, err
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawStatus, &result.Status); err != nil {
		return nil, err
	}
	if !result.Status.Ready || result.Status.ObservedGeneration != object.GetGeneration() {
		return nil, fmt.Errorf("node inventory is not ready")
	}
	return result, nil
}

func (p *Plugin) refreshInventory(ctx context.Context) error {
	object, err := p.dynamic.Resource(anchorkube.InventoryGVR).Get(ctx, p.config.NodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var spec model.AnchorNodeInventorySpec
	rawSpec, _, _ := unstructured.NestedMap(object.Object, "spec")
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawSpec, &spec); err != nil {
		return err
	}
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("list host network links: %w", err)
	}
	byMAC := map[string]netlink.Link{}
	for _, link := range links {
		if link.Attrs().HardwareAddr != nil {
			byMAC[strings.ToLower(link.Attrs().HardwareAddr.String())] = link
		}
	}
	status := model.AnchorNodeInventoryStatus{Ready: true, ObservedGeneration: object.GetGeneration(), Interfaces: map[string][]model.ENIPath{}}
	for profile, paths := range spec.Profiles {
		mapped := make([]model.ENIPath, 0, len(paths))
		for _, path := range paths {
			link := byMAC[strings.ToLower(path.MAC)]
			if link == nil {
				status.Ready = false
				status.Message = fmt.Sprintf("ENI %s (%s) is not present in the host network namespace", path.ENIID, path.MAC)
				continue
			}
			if err := netlink.LinkSetUp(link); err != nil {
				status.Ready = false
				status.Message = fmt.Sprintf("bring interface %s up: %v", link.Attrs().Name, err)
				continue
			}
			path.Interface = link.Attrs().Name
			mapped = append(mapped, path)
		}
		if len(mapped) == len(paths) {
			status.Interfaces[profile] = mapped
		}
	}
	rawStatus, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return err
	}
	copy := object.DeepCopy()
	copy.Object["status"] = rawStatus
	updated, err := p.dynamic.Resource(anchorkube.InventoryGVR).UpdateStatus(ctx, copy, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	_ = updated
	resources := resourcesForInventory(spec, status)
	if err := p.helper.PublishResources(ctx, resources); err != nil {
		return err
	}
	if !status.Ready {
		return errors.New(status.Message)
	}
	return nil
}

func resourcesForInventory(spec model.AnchorNodeInventorySpec, status model.AnchorNodeInventoryStatus) resourceslice.DriverResources {
	devices := []resourceapi.Device{}
	for profile, paths := range status.Interfaces {
		if len(paths) == 0 {
			continue
		}
		profileValue := profile
		azValue := spec.AZ
		pathSet := strings.Join(pathNames(paths), ",")
		for i := 0; i < spec.SlotsPerNode[profile]; i++ {
			devices = append(devices, resourceapi.Device{
				Name: fmt.Sprintf("%s-slot-%d", model.SafeName(profile), i),
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					resourceapi.QualifiedName(constants.DriverName + "/profile"):           {StringValue: &profileValue},
					resourceapi.QualifiedName(constants.DriverName + "/availability_zone"): {StringValue: &azValue},
					resourceapi.QualifiedName(constants.DriverName + "/paths"):             {StringValue: &pathSet},
				},
			})
		}
	}
	return resourceslice.DriverResources{Pools: map[string]resourceslice.Pool{spec.NodeName: {Slices: []resourceslice.Slice{{Devices: devices}}}}}
}

func emptyResources(nodeName string) resourceslice.DriverResources {
	return resourceslice.DriverResources{Pools: map[string]resourceslice.Pool{nodeName: {Slices: []resourceslice.Slice{{}}}}}
}

func pathNames(paths []model.ENIPath) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		result = append(result, path.Name)
	}
	return result
}
