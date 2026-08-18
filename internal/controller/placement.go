package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	Core     kubernetes.Interface
	Dynamic  dynamic.Interface
	Strategy anchoraws.BatchPlacementStrategy
	Logger   *slog.Logger
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
	endpoints := make([]anchoraws.Endpoint, len(targets))
	for i := range targets {
		endpoints[i] = targets[i].endpoint
	}
	results, err := r.Strategy.VerifyBatch(ctx, endpoints)
	if err != nil {
		return fmt.Errorf("verify Ready placements: %w", err)
	}
	if len(results) != len(targets) {
		return fmt.Errorf("verify Ready placements returned %d results for %d paths", len(results), len(targets))
	}
	drifted := map[int]bool{}
	for i, result := range results {
		if result.Err != nil || !result.Placed {
			drifted[targets[i].placement] = true
		}
	}
	works := []*placementWork{}
	for index := range drifted {
		placement := &live[index]
		anchormetrics.DriftDetections.WithLabelValues(placement.spec.Strategy).Inc()
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
		if claim == nil || claim.DeletionTimestamp != nil {
			if err := r.cleanupStalePlacement(ctx, object, claimUID); err != nil {
				r.Logger.Error("stale placement cleanup failed", "namespace", object.GetNamespace(), "name", object.GetName(), "error", err)
			}
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
		if !found || path.SubnetID != configured.SubnetID || !model.TagsMatch(path.Tags, configured.ENITagSelector) {
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
		paths = append(paths, model.BuildPlacementPath(address, path, configuredByName[address.Path]))
	}
	result = model.EndpointPlacementSpec{
		ClaimName: claim.Name, ClaimUID: string(claim.UID), NodeName: pod.Spec.NodeName,
		RequestName: allocation.Request, PoolName: allocation.Pool, DeviceName: allocation.Device,
		Strategy: class.Strategy, ForceSteal: strings.EqualFold(claim.Annotations[constants.ForceStealAnnotation], "true"), Paths: paths,
	}
	return result, nil
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
		withoutInterface := path
		withoutInterface.Interface = ""
		if !found || !reflect.DeepEqual(withoutInterface, expected) {
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
	endpoints := []anchoraws.Endpoint{}
	for _, work := range works {
		endpoints = append(endpoints, work.endpoints...)
	}
	results := r.Strategy.PlaceBatch(ctx, endpoints)
	if len(results) != len(endpoints) {
		return fmt.Errorf("placement returned %d results for %d paths", len(results), len(endpoints))
	}
	offset := 0
	var reconcileErrors []error
	for _, work := range works {
		placed := map[string]model.PlacementPath{}
		for _, path := range work.live.status.Paths {
			placed[path.Name] = path
		}
		var firstErr error
		for i, endpoint := range work.endpoints {
			pathErr := results[offset+i]
			ip, _ := ownershipAddress(endpoint.Path)
			if pathErr == nil {
				pathErr = r.markOwnershipPlaced(ctx, work.ownerships[ip], work.live.object.GetNamespace(), work.live.spec, endpoint.Path)
			}
			if pathErr == nil {
				pathErr = r.upsertNAD(ctx, work.live.object, work.live.spec.ClaimName, work.live.spec.ClaimUID, endpoint.Path)
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
			err := r.failWithPaths(ctx, work.live.object, firstErr, placementPaths(placed))
			r.Logger.Error("placement reconciliation failed", "namespace", work.live.object.GetNamespace(), "name", work.live.object.GetName(), "error", err)
			reconcileErrors = append(reconcileErrors, err)
			continue
		}
		alreadyReady := work.live.status.Phase == model.PlacementReady && work.live.status.ObservedGeneration == work.live.object.GetGeneration()
		if alreadyReady {
			continue
		}
		now := metav1.Now()
		status := model.EndpointPlacementStatus{Phase: model.PlacementReady, Message: "all endpoint paths are placed", ObservedGeneration: work.live.object.GetGeneration(), PlacedAt: &now, Paths: work.live.spec.Paths}
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
			byAddress[ip] = append(byAddress[ip], addressContender{key: key, namespace: placement.object.GetNamespace(), claimName: placement.spec.ClaimName})
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

func placementOwnerReference(placement *unstructured.Unstructured) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: constants.APIGroup + "/" + constants.APIVersion, Kind: "EndpointPlacement",
		Name: placement.GetName(), UID: placement.GetUID(), Controller: &controller,
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
	selector := constants.DriverName + "/claim-uid=" + claimUID
	nads, err := r.Dynamic.Resource(anchorkube.NADGVR).Namespace(object.GetNamespace()).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("list stale NetworkAttachmentDefinitions: %w", err)
	}
	if err == nil {
		for i := range nads.Items {
			if err := r.Dynamic.Resource(anchorkube.NADGVR).Namespace(object.GetNamespace()).Delete(ctx, nads.Items[i].GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete stale NetworkAttachmentDefinition %s: %w", nads.Items[i].GetName(), err)
			}
		}
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
	status := model.EndpointPlacementStatus{Phase: model.PlacementFailed, Message: cause.Error(), ObservedGeneration: object.GetGeneration()}
	if err := r.updateStatus(ctx, object, status); err != nil {
		return cause
	}
	if previousPhase != model.PlacementFailed {
		r.emit(ctx, claim.Namespace, claim.Name, claim.UID, corev1.EventTypeWarning, "EndpointPlacementFailed", cause.Error())
	}
	return cause
}

func (r *PlacementReconciler) failWithPaths(ctx context.Context, object *unstructured.Unstructured, cause error, paths []model.PlacementPath) error {
	previousPhase, _, _ := unstructured.NestedString(object.Object, "status", "phase")
	status := model.EndpointPlacementStatus{Phase: model.PlacementFailed, Message: cause.Error(), ObservedGeneration: object.GetGeneration(), Paths: paths}
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

func (r *PlacementReconciler) updateStatus(ctx context.Context, object *unstructured.Unstructured, status model.EndpointPlacementStatus) error {
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

func (r *PlacementReconciler) upsertNAD(ctx context.Context, placement *unstructured.Unstructured, claimName, claimUID string, path model.PlacementPath) error {
	namespace := placement.GetNamespace()
	name := NADName(claimName, path.Name)
	address := map[string]any{"address": path.IP}
	ipam := map[string]any{"type": "static", "addresses": []any{address}}
	if path.Gateway != "" {
		address["gateway"] = path.Gateway
		routes := make([]any, 0, len(path.Routes))
		for _, destination := range path.Routes {
			routes = append(routes, map[string]any{"dst": destination, "gw": path.Gateway})
		}
		ipam["routes"] = routes
	}
	config := map[string]any{
		"cniVersion": "0.3.1", "name": name,
		"plugins": []any{
			map[string]any{"type": "ipvlan", "master": path.Interface, "mode": "l2", "ipam": ipam},
			map[string]any{"type": "sbr"},
		},
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return err
	}
	desired := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "k8s.cni.cncf.io/v1", "kind": "NetworkAttachmentDefinition",
		"metadata": map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{constants.DriverName + "/claim-uid": claimUID}},
		"spec":     map[string]any{"config": string(encoded)},
	}}
	desired.SetOwnerReferences([]metav1.OwnerReference{placementOwnerReference(placement)})
	resource := r.Dynamic.Resource(anchorkube.NADGVR).Namespace(namespace)
	current, err := resource.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = resource.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	oldSpec, _, _ := unstructured.NestedMap(current.Object, "spec")
	if reflect.DeepEqual(oldSpec, desired.Object["spec"]) &&
		reflect.DeepEqual(current.GetLabels(), desired.GetLabels()) &&
		reflect.DeepEqual(current.GetOwnerReferences(), desired.GetOwnerReferences()) {
		return nil
	}
	current.Object["spec"] = desired.Object["spec"]
	current.SetLabels(desired.GetLabels())
	current.SetOwnerReferences(desired.GetOwnerReferences())
	_, err = resource.Update(ctx, current, metav1.UpdateOptions{})
	return err
}

func NADName(claimName, path string) string {
	name := model.SafeName("anchor", claimName, path)
	if len(name) <= 63 {
		return name
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))[:8]
	return strings.Trim(name[:54], "-") + "-" + hash
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
