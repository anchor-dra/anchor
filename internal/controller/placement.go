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
	claimsByName := make(map[string]*resourceapi.ResourceClaim, len(claims.Items))
	for i := range claims.Items {
		claim := &claims.Items[i]
		claimsByName[claim.Namespace+"/"+claim.Name] = claim
	}
	if err := r.migrateOwnerships(ctx, placements.Items); err != nil {
		return nil, nil, fmt.Errorf("migrate durable ownership: %w", err)
	}

	live := make([]livePlacement, 0, len(placements.Items))
	for i := range placements.Items {
		object := &placements.Items[i]
		spec, status, err := decodePlacement(object)
		if err != nil {
			_ = r.fail(ctx, object, err)
			continue
		}
		claim := claimsByName[object.GetNamespace()+"/"+spec.ClaimName]
		if claim == nil || string(claim.UID) != spec.ClaimUID || claim.DeletionTimestamp != nil {
			if err := r.cleanupStalePlacement(ctx, object, spec); err != nil {
				r.Logger.Error("stale placement cleanup failed", "namespace", object.GetNamespace(), "name", object.GetName(), "error", err)
			}
			continue
		}
		if err := r.ensurePlacementOwnerReference(ctx, object, claim); err != nil {
			r.Logger.Error("placement owner-reference repair failed", "namespace", object.GetNamespace(), "name", object.GetName(), "error", err)
			continue
		}
		live = append(live, livePlacement{object: object, spec: spec, status: status, claim: claim})
	}

	ownerships, err := r.listOwnerships(ctx)
	if err != nil {
		return nil, nil, err
	}
	conflicts := liveAddressConflicts(live, ownerships)
	return live, conflicts, nil
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

func (r *PlacementReconciler) reconcileOne(ctx context.Context, object *unstructured.Unstructured) error {
	spec, _, err := decodePlacement(object)
	if err != nil {
		return r.fail(ctx, object, err)
	}
	claim, err := r.Core.ResourceV1().ResourceClaims(object.GetNamespace()).Get(ctx, spec.ClaimName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return r.fail(ctx, object, fmt.Errorf("get ResourceClaim: %w", err))
	}
	if string(claim.UID) != spec.ClaimUID || claim.DeletionTimestamp != nil {
		return nil
	}
	return r.reconcileActive(ctx, object, claim)
}

func (r *PlacementReconciler) reconcileActive(ctx context.Context, object *unstructured.Unstructured, claim *resourceapi.ResourceClaim) error {
	spec, oldStatus, err := decodePlacement(object)
	if err != nil {
		return r.fail(ctx, object, err)
	}
	live := &livePlacement{object: object, spec: spec, status: oldStatus, claim: claim}
	work, err := r.prepareWork(ctx, live)
	if err != nil {
		return r.fail(ctx, object, err)
	}
	if work == nil {
		return nil
	}
	return r.executeWorks(ctx, []*placementWork{work})
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

func (r *PlacementReconciler) cleanupStalePlacement(ctx context.Context, object *unstructured.Unstructured, spec model.EndpointPlacementSpec) error {
	selector := constants.DriverName + "/claim-uid=" + spec.ClaimUID
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
	if err := r.Dynamic.Resource(anchorkube.PlacementGVR).Namespace(object.GetNamespace()).Delete(ctx, object.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete stale EndpointPlacement: %w", err)
	}
	return nil
}

func (r *PlacementReconciler) fail(ctx context.Context, object *unstructured.Unstructured, cause error) error {
	return r.failWithPaths(ctx, object, cause, nil)
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
	config := map[string]any{
		"cniVersion": "0.3.1", "name": name,
		"plugins": []any{
			map[string]any{"type": "ipvlan", "master": path.Interface, "mode": "l2", "ipam": map[string]any{"type": "static", "addresses": []any{map[string]any{"address": path.IP}}}},
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
