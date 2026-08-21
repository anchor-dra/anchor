package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	anchoraws "github.com/anchor-dra/anchor/internal/aws"
	"github.com/anchor-dra/anchor/internal/constants"
	anchorkube "github.com/anchor-dra/anchor/internal/kube"
	anchormetrics "github.com/anchor-dra/anchor/internal/metrics"
	"github.com/anchor-dra/anchor/internal/model"
)

type PlacementReconciler struct {
	Core         kubernetes.Interface
	Dynamic      dynamic.Interface
	Strategy     anchoraws.BatchPlacementStrategy
	StrategyName string
	Strategies   map[string]anchoraws.BatchPlacementStrategy
	Logger       *slog.Logger
}

type livePlacement struct {
	object *unstructured.Unstructured
	spec   model.EndpointPlacementSpec
	status model.EndpointPlacementStatus
	claim  *resourceapi.ResourceClaim
}

type placementWork struct {
	live       *livePlacement
	ownerships map[string]*ownershipRecord
	endpoints  []anchoraws.Endpoint
}

func (r *PlacementReconciler) Reconcile(ctx context.Context) error {
	live, conflicts, err := r.loadLive(ctx)
	if err != nil {
		return err
	}
	works := []*placementWork{}
	for i := range live {
		placement := &live[i]
		key := placement.object.GetNamespace() + "/" + placement.object.GetName()
		if message := conflicts[key]; message != "" {
			_ = r.fail(ctx, placement.object, fmt.Errorf("%s", message))
			continue
		}
		if err := r.reconcileConfigurationCondition(ctx, placement); err != nil {
			r.Logger.Warn("DeviceClass configuration drift check failed", "namespace", placement.object.GetNamespace(), "name", placement.object.GetName(), "error", err)
		}
		if placement.status.Phase == model.PlacementReady && placement.status.ObservedGeneration == placement.object.GetGeneration() {
			continue
		}
		work, err := r.prepareWork(ctx, placement)
		if err != nil {
			_ = r.fail(ctx, placement.object, err)
			continue
		}
		if work != nil {
			works = append(works, work)
		}
	}
	r.executeWorks(ctx, works)
	return nil
}

func (r *PlacementReconciler) reconcileConfigurationCondition(ctx context.Context, placement *livePlacement) error {
	requestName := placement.spec.RequestName
	className := ""
	for _, request := range placement.claim.Spec.Devices.Requests {
		if request.Name == requestName && request.Exactly != nil {
			className = request.Exactly.DeviceClassName
			break
		}
	}
	if className == "" {
		return nil
	}
	current, err := r.Core.ResourceV1().DeviceClasses().Get(ctx, className, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var currentParams *model.DeviceClassParameters
	for _, config := range current.Spec.Config {
		if config.Opaque == nil || config.Opaque.Driver != constants.DriverName {
			continue
		}
		decoded, decodeErr := model.Decode[model.DeviceClassParameters](config.Opaque.Parameters.Raw)
		if decodeErr != nil {
			return decodeErr
		}
		if err := decoded.NormalizeAndValidate(); err != nil {
			return err
		}
		currentParams = &decoded
		break
	}
	if currentParams == nil {
		return fmt.Errorf("DeviceClass %s has no Anchor configuration", className)
	}
	allocated, _, err := anchorkube.AllocationParameters(placement.claim)
	if err != nil {
		return err
	}
	currentStatus := metav1.ConditionTrue
	reason, message := "Current", "allocated DeviceClass configuration matches the current class"
	if !deviceClassEquivalent(allocated, *currentParams) {
		currentStatus, reason, message = metav1.ConditionFalse, "DeviceClassChanged", "allocated DeviceClass configuration is stale; reallocate this ResourceClaim before activating topology changes"
	}
	conditions := append([]metav1.Condition(nil), placement.status.Conditions...)
	apiMeta.SetStatusCondition(&conditions, metav1.Condition{Type: "ConfigurationCurrent", Status: currentStatus, Reason: reason, Message: message, ObservedGeneration: placement.object.GetGeneration()})
	if reflect.DeepEqual(conditions, placement.status.Conditions) {
		return nil
	}
	status := placement.status
	status.Conditions = conditions
	if err := r.updateStatus(ctx, placement.object, status); err != nil {
		return err
	}
	placement.status = status
	return nil
}

func deviceClassEquivalent(left, right model.DeviceClassParameters) bool {
	canonicalize := func(value model.DeviceClassParameters) model.DeviceClassParameters {
		value.Paths = append([]model.PathSpec(nil), value.Paths...)
		for i := range value.Paths {
			path := &value.Paths[i]
			path.Subnets = append([]model.AWSSubnetSpec(nil), path.Subnets...)
			path.RouteTableIDs = append([]string(nil), path.RouteTableIDs...)
			path.Routes = append([]string(nil), path.Routes...)
			sort.Slice(path.Subnets, func(i, j int) bool { return path.Subnets[i].SubnetID < path.Subnets[j].SubnetID })
			sort.Strings(path.RouteTableIDs)
			sort.Strings(path.Routes)
		}
		sort.Slice(value.Paths, func(i, j int) bool { return value.Paths[i].Name < value.Paths[j].Name })
		return value
	}
	return reflect.DeepEqual(canonicalize(left), canonicalize(right))
}

func (r *PlacementReconciler) VerifyReady(ctx context.Context) error {
	live, conflicts, err := r.loadLive(ctx)
	if err != nil {
		return err
	}
	type verificationTarget struct {
		placement int
		endpoint  anchoraws.Endpoint
	}
	targets := []verificationTarget{}
	for i := range live {
		placement := &live[i]
		key := placement.object.GetNamespace() + "/" + placement.object.GetName()
		if conflicts[key] != "" || placement.status.Phase != model.PlacementReady || placement.status.ObservedGeneration != placement.object.GetGeneration() || len(placement.claim.Status.ReservedFor) != 1 {
			continue
		}
		for _, path := range placement.spec.Paths {
			targets = append(targets, verificationTarget{placement: i, endpoint: anchoraws.Endpoint{
				ClaimUID: placement.spec.ClaimUID, ClaimName: placement.object.GetNamespace() + "/" + placement.spec.ClaimName,
				NodeName: placement.spec.NodeName, Path: path, ForceSteal: placement.spec.ForceSteal,
			}})
		}
	}
	byStrategy := map[string][]verificationTarget{}
	for _, target := range targets {
		byStrategy[live[target.placement].spec.Strategy] = append(byStrategy[live[target.placement].spec.Strategy], target)
	}
	drifted := map[int]map[string]bool{}
	for strategyName, strategyTargets := range byStrategy {
		strategy, ok := r.strategyFor(strategyName)
		if !ok {
			return fmt.Errorf("verify Ready placements: strategy %q is not enabled", strategyName)
		}
		endpoints := make([]anchoraws.Endpoint, len(strategyTargets))
		for i := range strategyTargets {
			endpoints[i] = strategyTargets[i].endpoint
		}
		results, verifyErr := strategy.VerifyBatch(ctx, endpoints)
		if verifyErr != nil {
			return fmt.Errorf("verify Ready %s placements: %w", strategyName, verifyErr)
		}
		if len(results) != len(strategyTargets) {
			return fmt.Errorf("verify Ready %s placements returned %d results for %d paths", strategyName, len(results), len(strategyTargets))
		}
		for i, result := range results {
			if result.Err != nil || !result.Placed {
				target := strategyTargets[i]
				if drifted[target.placement] == nil {
					drifted[target.placement] = map[string]bool{}
				}
				drifted[target.placement][target.endpoint.Path.Name] = true
			}
		}
	}
	works := []*placementWork{}
	for index, paths := range drifted {
		placement := &live[index]
		anchormetrics.DriftDetections.WithLabelValues(placement.spec.Strategy).Inc()
		work, err := r.prepareWorkPaths(ctx, placement, paths)
		if err != nil {
			_ = r.fail(ctx, placement.object, err)
			continue
		}
		if work != nil {
			works = append(works, work)
		}
	}
	r.executeWorks(ctx, works)
	return nil
}

func (r *PlacementReconciler) loadLive(ctx context.Context) ([]livePlacement, map[string]string, error) {
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	placements, err := r.Dynamic.Resource(anchorkube.PlacementGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("list EndpointPlacements: %w", err)
	}
	claims, err := r.Core.ResourceV1().ResourceClaims(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("list ResourceClaims: %w", err)
	}
	inventories, err := r.Dynamic.Resource(anchorkube.InventoryGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("list AnchorNodeInventories: %w", err)
	}
	claimsByUID := make(map[string]*resourceapi.ResourceClaim, len(claims.Items))
	for i := range claims.Items {
		claim := &claims.Items[i]
		claimsByUID[claim.Namespace+"/"+string(claim.UID)] = claim
	}
	inventoriesByName := make(map[string]*unstructured.Unstructured, len(inventories.Items))
	for i := range inventories.Items {
		inventory := &inventories.Items[i]
		inventoriesByName[inventory.GetName()] = inventory
	}

	live := make([]livePlacement, 0, len(placements.Items))
	podCache := map[string]podLookupResult{}
	for i := range placements.Items {
		object := &placements.Items[i]
		requested, status, err := decodePlacement(object)
		if err != nil {
			_ = r.fail(ctx, object, err)
			continue
		}
		claimUID, canonical := placementClaimUID(object.GetName())
		if !canonical {
			if err := r.deletePlacement(ctx, object); err != nil {
				r.Logger.Error("non-canonical placement cleanup failed", "namespace", object.GetNamespace(), "name", object.GetName(), "error", err)
			}
			continue
		}
		claim := claimsByUID[object.GetNamespace()+"/"+claimUID]
		if claim == nil {
			if err := r.cleanupStalePlacement(ctx, object, claimUID); err != nil {
				r.Logger.Error("stale placement cleanup failed", "namespace", object.GetNamespace(), "name", object.GetName(), "error", err)
			}
			continue
		}
		if claim.DeletionTimestamp != nil {
			continue
		}
		if err := r.ensurePlacementOwnerReference(ctx, object, claim); err != nil {
			r.Logger.Error("placement owner-reference repair failed", "namespace", object.GetNamespace(), "name", object.GetName(), "error", err)
			continue
		}
		if len(claim.Status.ReservedFor) == 0 {
			continue
		}
		pod, handoff, err := r.getReservedPod(ctx, claim, podCache)
		if err != nil {
			_ = r.failForClaim(ctx, object, claim, err)
			continue
		}
		if handoff {
			continue
		}
		spec, err := derivePlacement(claim, pod, inventoriesByName)
		if err != nil {
			_ = r.failForClaim(ctx, object, claim, err)
			continue
		}
		if !placementRequestMatches(requested, spec) {
			_ = r.failForClaim(ctx, object, claim, fmt.Errorf("EndpointPlacement spec does not match the claim allocation, reserved Pod, and node inventory"))
			continue
		}
		live = append(live, livePlacement{object: object, spec: spec, status: status, claim: claim})
	}
	if err := r.migrateOwnerships(ctx, live); err != nil {
		return nil, nil, fmt.Errorf("migrate durable ownership: %w", err)
	}

	ownerships, err := r.listOwnerships(ctx)
	if err != nil {
		return nil, nil, err
	}
	conflicts := liveAddressConflicts(live, ownerships)
	return live, conflicts, nil
}

func placementClaimUID(name string) (string, bool) {
	uid := strings.TrimPrefix(name, "claim-")
	return uid, uid != "" && uid != name
}

func decodePlacement(object *unstructured.Unstructured) (model.EndpointPlacementSpec, model.EndpointPlacementStatus, error) {
	var spec model.EndpointPlacementSpec
	var status model.EndpointPlacementStatus
	rawSpec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil || !found {
		return spec, status, fmt.Errorf("EndpointPlacement has no spec")
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawSpec, &spec); err != nil {
		return spec, status, fmt.Errorf("decode placement spec: %w", err)
	}
	if rawStatus, found, _ := unstructured.NestedMap(object.Object, "status"); found {
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawStatus, &status); err != nil {
			return spec, status, fmt.Errorf("decode placement status: %w", err)
		}
	}
	return spec, status, nil
}

type placementInventory struct {
	Spec       model.AnchorNodeInventorySpec
	Status     model.AnchorNodeInventoryStatus
	Generation int64
}

type podLookupResult struct {
	pod *corev1.Pod
	err error
}

func (r *PlacementReconciler) getReservedPod(ctx context.Context, claim *resourceapi.ResourceClaim, cache map[string]podLookupResult) (*corev1.Pod, bool, error) {
	if len(claim.Status.ReservedFor) != 1 {
		return nil, false, fmt.Errorf("claim must have exactly one active Pod reservation, found %d", len(claim.Status.ReservedFor))
	}
	reservation := claim.Status.ReservedFor[0]
	if reservation.APIGroup != "" || reservation.Resource != "pods" {
		return nil, false, fmt.Errorf("claim reservation must reference one core Pod")
	}
	key := claim.Namespace + "/" + reservation.Name
	result, found := cache[key]
	if !found {
		result.pod, result.err = r.Core.CoreV1().Pods(claim.Namespace).Get(ctx, reservation.Name, metav1.GetOptions{})
		cache[key] = result
	}
	if apierrors.IsNotFound(result.err) {
		return nil, true, nil
	}
	if result.err != nil {
		return nil, false, fmt.Errorf("get reserving Pod %s: %w", key, result.err)
	}
	if result.pod.UID != reservation.UID {
		return nil, true, nil
	}
	if result.pod.DeletionTimestamp != nil {
		return nil, true, nil
	}
	return result.pod, false, nil
}

func derivePlacement(claim *resourceapi.ResourceClaim, pod *corev1.Pod, inventories map[string]*unstructured.Unstructured) (model.EndpointPlacementSpec, error) {
	var result model.EndpointPlacementSpec
	if len(claim.Status.ReservedFor) != 1 {
		return result, fmt.Errorf("claim must have exactly one active Pod reservation, found %d", len(claim.Status.ReservedFor))
	}
	reservation := claim.Status.ReservedFor[0]
	if reservation.APIGroup != "" || reservation.Resource != "pods" {
		return result, fmt.Errorf("claim reservation must reference one core Pod")
	}
	if pod == nil || pod.Namespace != claim.Namespace || pod.Name != reservation.Name || pod.UID != reservation.UID {
		return result, fmt.Errorf("claim reservation does not match an existing Pod UID")
	}
	if pod.DeletionTimestamp != nil {
		return result, fmt.Errorf("reserving Pod %s is terminating", pod.Name)
	}
	if pod.Spec.NodeName == "" {
		return result, fmt.Errorf("reserving Pod %s is not assigned to a node", pod.Name)
	}
	class, params, err := anchorkube.AllocationParameters(claim)
	if err != nil {
		return result, err
	}
	allocation, err := anchorkube.AllocationForDriver(claim)
	if err != nil {
		return result, err
	}
	if allocation.Pool != pod.Spec.NodeName {
		return result, fmt.Errorf("allocated pool %q does not match reserving Pod node %q", allocation.Pool, pod.Spec.NodeName)
	}
	inventoryObject := inventories[pod.Spec.NodeName]
	if inventoryObject == nil {
		return result, fmt.Errorf("node %s has no AnchorNodeInventory", pod.Spec.NodeName)
	}
	inventory, err := decodePlacementInventory(inventoryObject)
	if err != nil {
		return result, err
	}
	if inventory.Spec.NodeName != pod.Spec.NodeName || inventoryObject.GetName() != pod.Spec.NodeName {
		return result, fmt.Errorf("inventory identity does not match reserving Pod node %q", pod.Spec.NodeName)
	}
	if !inventory.Status.Ready || inventory.Status.ObservedGeneration != inventory.Generation {
		return result, fmt.Errorf("node inventory %s is not Ready for its current generation", pod.Spec.NodeName)
	}
	authoritative, mapped, err := trustedInventoryPaths(inventory, class.Profile)
	if err != nil {
		return result, err
	}
	if len(authoritative) != len(class.Paths) {
		return result, fmt.Errorf("profile %q inventory does not match its allocated DeviceClass", class.Profile)
	}
	subnetCIDRs := make(map[string]string, len(authoritative))
	configuredByName := make(map[string]model.PathSpec, len(class.Paths))
	for _, configured := range class.Paths {
		path, found := authoritative[configured.Name]
		matches := found
		if class.Strategy == model.StrategyL2Announce {
			matches = matches && path.Interface == configured.ParentInterface && path.SubnetCIDR == configured.SubnetCIDR
		} else {
			_, allowedSubnet := configured.ResolveAWSSubnet(path.SubnetID)
			matches = matches && allowedSubnet && model.TagsMatch(path.Tags, configured.ENITagSelector)
		}
		if !matches {
			return result, fmt.Errorf("path %q inventory does not match its allocated DeviceClass", configured.Name)
		}
		configuredByName[configured.Name] = configured
		subnetCIDRs[path.SubnetID] = path.SubnetCIDR
	}
	if err := params.Validate(class, subnetCIDRs); err != nil {
		return result, err
	}
	paths := make([]model.PlacementPath, 0, len(params.Addresses))
	for _, address := range params.Addresses {
		path, found := authoritative[address.Path]
		if !found {
			return result, fmt.Errorf("path %q is missing from controller inventory", address.Path)
		}
		path.Interface = mapped[address.Path].Interface
		path.MAC = mapped[address.Path].MAC
		if err := validateResolvedGateway(configuredByName[address.Path], path); err != nil {
			return result, err
		}
		paths = append(paths, model.BuildPlacementPath(address, path, configuredByName[address.Path]))
	}
	result = model.EndpointPlacementSpec{
		ClaimName: claim.Name, ClaimUID: string(claim.UID), OwnershipName: anchorkube.ClaimOwnershipName(claim, pod), NodeName: pod.Spec.NodeName,
		RequestName: allocation.Request, PoolName: allocation.Pool, DeviceName: allocation.Device,
		Strategy: class.Strategy, ForceSteal: strings.EqualFold(claim.Annotations[constants.ForceStealAnnotation], "true"), Paths: paths,
	}
	return result, nil
}

func validateResolvedGateway(configured model.PathSpec, path model.ENIPath) error {
	resolved, ok := configured.ResolveAWSSubnet(path.SubnetID)
	if !ok || resolved.Gateway == "" {
		return nil
	}
	subnet, err := netip.ParsePrefix(path.SubnetCIDR)
	if err != nil {
		return fmt.Errorf("path %q has invalid discovered subnet CIDR %q", configured.Name, path.SubnetCIDR)
	}
	gateway, _ := netip.ParseAddr(resolved.Gateway)
	if !subnet.Contains(gateway) {
		return fmt.Errorf("path %q gateway %s is outside subnet %s", configured.Name, gateway, subnet)
	}
	return nil
}

func decodePlacementInventory(object *unstructured.Unstructured) (*placementInventory, error) {
	result := &placementInventory{Generation: object.GetGeneration()}
	rawSpec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("AnchorNodeInventory %s has no spec", object.GetName())
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawSpec, &result.Spec); err != nil {
		return nil, fmt.Errorf("decode AnchorNodeInventory %s spec: %w", object.GetName(), err)
	}
	rawStatus, found, err := unstructured.NestedMap(object.Object, "status")
	if err != nil || !found {
		return nil, fmt.Errorf("AnchorNodeInventory %s has no status", object.GetName())
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawStatus, &result.Status); err != nil {
		return nil, fmt.Errorf("decode AnchorNodeInventory %s status: %w", object.GetName(), err)
	}
	return result, nil
}

func trustedInventoryPaths(inventory *placementInventory, profile string) (map[string]model.ENIPath, map[string]model.ENIPath, error) {
	specPaths := inventory.Spec.Profiles[profile]
	statusPaths := inventory.Status.Interfaces[profile]
	if len(specPaths) == 0 || len(statusPaths) != len(specPaths) {
		return nil, nil, fmt.Errorf("profile %q is not completely mapped in node inventory", profile)
	}
	authoritative := make(map[string]model.ENIPath, len(specPaths))
	for _, path := range specPaths {
		if path.Name == "" || authoritative[path.Name].Name != "" {
			return nil, nil, fmt.Errorf("profile %q has an invalid or duplicate controller inventory path", profile)
		}
		authoritative[path.Name] = path
	}
	mapped := make(map[string]model.ENIPath, len(statusPaths))
	for _, path := range statusPaths {
		if path.Interface == "" || mapped[path.Name].Name != "" {
			return nil, nil, fmt.Errorf("profile %q has an invalid or duplicate node interface mapping", profile)
		}
		expected, found := authoritative[path.Name]
		reported := path
		if expected.Interface == "" {
			reported.Interface = ""
		} else {
			if reported.Interface != expected.Interface || reported.MAC == "" {
				return nil, nil, fmt.Errorf("node-reported path %q does not match the approved host parent", path.Name)
			}
			reported.MAC = ""
		}
		if !found || !reflect.DeepEqual(reported, expected) {
			return nil, nil, fmt.Errorf("node-reported path %q does not match controller inventory", path.Name)
		}
		mapped[path.Name] = path
	}
	return authoritative, mapped, nil
}

func placementRequestMatches(requested, expected model.EndpointPlacementSpec) bool {
	requested.ForceSteal = false
	expected.ForceSteal = false
	return reflect.DeepEqual(requested, expected)
}

func (r *PlacementReconciler) prepareWork(ctx context.Context, placement *livePlacement) (*placementWork, error) {
	return r.prepareWorkPaths(ctx, placement, nil)
}

func (r *PlacementReconciler) prepareWorkPaths(ctx context.Context, placement *livePlacement, selected map[string]bool) (*placementWork, error) {
	// A standalone claim is briefly unreserved while its old pod is removed and
	// its replacement is being scheduled. Keep the last successful placement as
	// the ownership record during that handoff; clearing it would make Anchor
	// mistake its own ENI assignment for an external conflict.
	if len(placement.claim.Status.ReservedFor) == 0 {
		return nil, nil
	}
	if len(placement.claim.Status.ReservedFor) != 1 {
		return nil, fmt.Errorf("claim must have exactly one active pod reservation, found %d", len(placement.claim.Status.ReservedFor))
	}
	if _, ok := r.strategyFor(placement.spec.Strategy); !ok {
		return nil, fmt.Errorf("placement strategy %q is not enabled by this controller", placement.spec.Strategy)
	}
	ownerships, err := r.reserveOwnerships(ctx, placement.object.GetNamespace(), placement.spec)
	if err != nil {
		return nil, err
	}
	previous := map[string]string{}
	for _, path := range placement.status.Paths {
		previous[path.Name] = path.ENIID
	}
	work := &placementWork{live: placement, ownerships: ownerships, endpoints: make([]anchoraws.Endpoint, 0, len(placement.spec.Paths))}
	for _, path := range placement.spec.Paths {
		if selected != nil && !selected[path.Name] {
			continue
		}
		ip, _ := ownershipAddress(path)
		previousENI := previous[path.Name]
		if ownership := ownerships[ip]; ownership != nil && ownership.Status.ENIID != "" {
			previousENI = ownership.Status.ENIID
		}
		work.endpoints = append(work.endpoints, anchoraws.Endpoint{
			ClaimUID: placement.spec.ClaimUID, ClaimName: placement.object.GetNamespace() + "/" + placement.spec.ClaimName,
			NodeName: placement.spec.NodeName, Path: path, PreviousENI: previousENI, ForceSteal: placement.spec.ForceSteal,
		})
	}
	return work, nil
}

func (r *PlacementReconciler) executeWorks(ctx context.Context, works []*placementWork) error {
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	grouped := map[string][]*placementWork{}
	for _, work := range works {
		grouped[work.live.spec.Strategy] = append(grouped[work.live.spec.Strategy], work)
	}
	var errs []error
	for strategyName, strategyWorks := range grouped {
		strategy, ok := r.strategyFor(strategyName)
		if !ok {
			errs = append(errs, fmt.Errorf("placement strategy %q is not enabled", strategyName))
			continue
		}
		if err := r.executeStrategyWorks(ctx, strategy, strategyWorks); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *PlacementReconciler) executeStrategyWorks(ctx context.Context, strategy anchoraws.BatchPlacementStrategy, works []*placementWork) error {
	endpoints := []anchoraws.Endpoint{}
	for _, work := range works {
		endpoints = append(endpoints, work.endpoints...)
	}
	results := strategy.PlaceBatch(ctx, endpoints)
	if len(results) != len(endpoints) {
		return fmt.Errorf("placement returned %d results for %d paths", len(results), len(endpoints))
	}
	offset := 0
	var reconcileErrors []error
	for _, work := range works {
		placed := map[string]model.PlacementPath{}
		routeTables := routeStatusesByKey(work.live.status.RouteTables)
		for _, path := range work.live.status.Paths {
			placed[path.Name] = path
		}
		var firstErr error
		for i, endpoint := range work.endpoints {
			pathErr := results[offset+i].Err
			for _, status := range results[offset+i].RouteTables {
				routeTables[routeStatusKey(status)] = status
			}
			ip, _ := ownershipAddress(endpoint.Path)
			if pathErr == nil {
				pathErr = r.markOwnershipPlaced(ctx, work.ownerships[ip], work.live.object.GetNamespace(), work.live.spec, endpoint.Path)
			}
			if pathErr != nil {
				if firstErr == nil {
					firstErr = pathErr
				}
				continue
			}
			placed[endpoint.Path.Name] = endpoint.Path
		}
		offset += len(work.endpoints)
		if firstErr != nil {
			err := r.failWithPathsAndRoutes(ctx, work.live.object, firstErr, placementPaths(placed), sortedRouteStatuses(routeTables))
			r.Logger.Error("placement reconciliation failed", "namespace", work.live.object.GetNamespace(), "name", work.live.object.GetName(), "error", err)
			reconcileErrors = append(reconcileErrors, err)
			continue
		}
		alreadyReady := work.live.status.Phase == model.PlacementReady && work.live.status.ObservedGeneration == work.live.object.GetGeneration()
		if alreadyReady {
			continue
		}
		now := metav1.Now()
		status := model.EndpointPlacementStatus{
			Phase: model.PlacementReady, Message: "all endpoint paths are placed", ObservedGeneration: work.live.object.GetGeneration(),
			PlacedAt: &now, Paths: work.live.spec.Paths, Injection: work.live.status.Injection,
			RouteTables: sortedRouteStatuses(routeTables), Conditions: work.live.status.Conditions,
		}
		if err := r.updateStatus(ctx, work.live.object, status); err != nil {
			reconcileErrors = append(reconcileErrors, err)
			continue
		}
		if work.live.status.Phase != model.PlacementReady {
			claim := work.live.claim
			r.emit(ctx, claim.Namespace, claim.Name, claim.UID, corev1.EventTypeNormal, "EndpointPlaced", fmt.Sprintf("placed %d static endpoint paths on node %s", len(work.live.spec.Paths), work.live.spec.NodeName))
		}
	}
	return errors.Join(reconcileErrors...)
}

func (r *PlacementReconciler) strategyFor(name string) (anchoraws.BatchPlacementStrategy, bool) {
	if strategy := r.Strategies[name]; strategy != nil {
		return strategy, true
	}
	strategyName := r.StrategyName
	if strategyName == "" {
		strategyName = model.StrategyIPReassign
	}
	return r.Strategy, r.Strategy != nil && name == strategyName
}

type addressContender struct {
	key       string
	namespace string
	claimName string
}

func liveAddressConflicts(placements []livePlacement, ownerships map[string]*ownershipRecord) map[string]string {
	byAddress := map[string][]addressContender{}
	for i := range placements {
		placement := &placements[i]
		key := placement.object.GetNamespace() + "/" + placement.object.GetName()
		for _, path := range placement.spec.Paths {
			ip, err := ownershipAddress(path)
			if err != nil {
				continue
			}
			byAddress[ip] = append(byAddress[ip], addressContender{key: key, namespace: placement.object.GetNamespace(), claimName: ownershipClaimName(placement.spec)})
		}
	}
	conflicts := map[string]string{}
	for address, contenders := range byAddress {
		if len(contenders) < 2 {
			continue
		}
		established := ownerships[address]
		winner := ""
		matches := 0
		if established != nil {
			for _, contender := range contenders {
				if sameLogicalOwner(established.Spec, contender.namespace, contender.claimName) {
					winner = contender.key
					matches++
				}
			}
		}
		for _, contender := range contenders {
			if matches == 1 && contender.key == winner {
				continue
			}
			if established == nil {
				conflicts[contender.key] = fmt.Sprintf("address %s is declared by more than one live ResourceClaim and has no established owner", address)
			} else {
				conflicts[contender.key] = fmt.Sprintf("address %s is declared by more than one live ResourceClaim; established owner is %s/%s", address, established.Spec.ClaimNamespace, established.Spec.ClaimName)
			}
		}
	}
	return conflicts
}

func claimOwnerReference(claim *resourceapi.ResourceClaim) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: "resource.k8s.io/v1", Kind: "ResourceClaim", Name: claim.Name,
		UID: claim.UID, Controller: &controller,
	}
}

func (r *PlacementReconciler) ensurePlacementOwnerReference(ctx context.Context, object *unstructured.Unstructured, claim *resourceapi.ResourceClaim) error {
	desired := []metav1.OwnerReference{claimOwnerReference(claim)}
	if reflect.DeepEqual(object.GetOwnerReferences(), desired) {
		return nil
	}
	copy := object.DeepCopy()
	copy.SetOwnerReferences(desired)
	updated, err := r.Dynamic.Resource(anchorkube.PlacementGVR).Namespace(object.GetNamespace()).Update(ctx, copy, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	*object = *updated
	return nil
}

func (r *PlacementReconciler) cleanupStalePlacement(ctx context.Context, object *unstructured.Unstructured, claimUID string) error {
	if claimUID == "" {
		return fmt.Errorf("stale EndpointPlacement has no claim UID")
	}
	return r.deletePlacement(ctx, object)
}

func (r *PlacementReconciler) deletePlacement(ctx context.Context, object *unstructured.Unstructured) error {
	if err := r.Dynamic.Resource(anchorkube.PlacementGVR).Namespace(object.GetNamespace()).Delete(ctx, object.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete stale EndpointPlacement: %w", err)
	}
	return nil
}

func (r *PlacementReconciler) fail(ctx context.Context, object *unstructured.Unstructured, cause error) error {
	return r.failWithPaths(ctx, object, cause, nil)
}

func (r *PlacementReconciler) failForClaim(ctx context.Context, object *unstructured.Unstructured, claim *resourceapi.ResourceClaim, cause error) error {
	previousPhase, _, _ := unstructured.NestedString(object.Object, "status", "phase")
	status := statusWithContinuity(object, model.EndpointPlacementStatus{Phase: model.PlacementFailed, Message: cause.Error(), ObservedGeneration: object.GetGeneration()})
	if err := r.updateStatus(ctx, object, status); err != nil {
		return cause
	}
	if previousPhase != model.PlacementFailed {
		r.emit(ctx, claim.Namespace, claim.Name, claim.UID, corev1.EventTypeWarning, "EndpointPlacementFailed", cause.Error())
	}
	return cause
}

func (r *PlacementReconciler) failWithPaths(ctx context.Context, object *unstructured.Unstructured, cause error, paths []model.PlacementPath) error {
	return r.failWithPathsAndRoutes(ctx, object, cause, paths, nil)
}

func (r *PlacementReconciler) failWithPathsAndRoutes(ctx context.Context, object *unstructured.Unstructured, cause error, paths []model.PlacementPath, routeTables []model.RouteTableStatus) error {
	previousPhase, _, _ := unstructured.NestedString(object.Object, "status", "phase")
	status := statusWithContinuity(object, model.EndpointPlacementStatus{Phase: model.PlacementFailed, Message: cause.Error(), ObservedGeneration: object.GetGeneration(), Paths: paths, RouteTables: routeTables})
	if err := r.updateStatus(ctx, object, status); err != nil {
		return cause
	}
	claimName := object.GetName()
	claimUID := object.GetUID()
	if rawSpec, found, _ := unstructured.NestedMap(object.Object, "spec"); found {
		var spec model.EndpointPlacementSpec
		if runtime.DefaultUnstructuredConverter.FromUnstructured(rawSpec, &spec) == nil {
			claimName = spec.ClaimName
			claimUID = types.UID(spec.ClaimUID)
		}
	}
	if previousPhase != model.PlacementFailed {
		r.emit(ctx, object.GetNamespace(), claimName, claimUID, corev1.EventTypeWarning, "EndpointPlacementFailed", cause.Error())
	}
	return cause
}

func statusWithContinuity(object *unstructured.Unstructured, status model.EndpointPlacementStatus) model.EndpointPlacementStatus {
	_, previous, err := decodePlacement(object)
	if err != nil {
		return status
	}
	status.Injection = previous.Injection
	status.Conditions = previous.Conditions
	return status
}

func (r *PlacementReconciler) updateStatus(ctx context.Context, object *unstructured.Unstructured, status model.EndpointPlacementStatus) error {
	var previous model.EndpointPlacementStatus
	if currentRaw, found, _ := unstructured.NestedMap(object.Object, "status"); found {
		_ = runtime.DefaultUnstructuredConverter.FromUnstructured(currentRaw, &previous)
	}
	if status.Injection == nil {
		status.Injection = previous.Injection
	}
	if status.RouteTables == nil {
		status.RouteTables = previous.RouteTables
	}
	if status.Conditions == nil {
		status.Conditions = previous.Conditions
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return err
	}
	current, _, _ := unstructured.NestedMap(object.Object, "status")
	if reflect.DeepEqual(current, raw) {
		return nil
	}
	copy := object.DeepCopy()
	copy.Object["status"] = raw
	_, err = r.Dynamic.Resource(anchorkube.PlacementGVR).Namespace(object.GetNamespace()).UpdateStatus(ctx, copy, metav1.UpdateOptions{})
	return err
}

func routeStatusKey(status model.RouteTableStatus) string {
	return status.PathName + "\x00" + status.RouteTableID
}

func routeStatusesByKey(statuses []model.RouteTableStatus) map[string]model.RouteTableStatus {
	result := make(map[string]model.RouteTableStatus, len(statuses))
	for _, status := range statuses {
		result[routeStatusKey(status)] = status
	}
	return result
}

func sortedRouteStatuses(values map[string]model.RouteTableStatus) []model.RouteTableStatus {
	result := make([]model.RouteTableStatus, 0, len(values))
	for _, status := range values {
		result = append(result, status)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].PathName == result[j].PathName {
			return result[i].RouteTableID < result[j].RouteTableID
		}
		return result[i].PathName < result[j].PathName
	})
	return result
}

func placementPaths(values map[string]model.PlacementPath) []model.PlacementPath {
	paths := make([]model.PlacementPath, 0, len(values))
	for _, path := range values {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i].Name < paths[j].Name })
	return paths
}

func (r *PlacementReconciler) emit(ctx context.Context, namespace, name string, uid types.UID, eventType, reason, message string) {
	now := metav1.NewTime(time.Now())
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{GenerateName: "anchor-", Namespace: namespace},
		InvolvedObject: corev1.ObjectReference{APIVersion: "resource.k8s.io/v1", Kind: "ResourceClaim", Namespace: namespace, Name: name, UID: uid},
		Type:           eventType, Reason: reason, Message: message, FirstTimestamp: now, LastTimestamp: now, Count: 1, Source: corev1.EventSource{Component: "anchor-controller"},
	}
	_, _ = r.Core.CoreV1().Events(namespace).Create(ctx, event, metav1.CreateOptions{})
}
