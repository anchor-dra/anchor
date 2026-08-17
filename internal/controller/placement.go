package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	anchoraws "example.com/anchor/internal/aws"
	"example.com/anchor/internal/constants"
	anchorkube "example.com/anchor/internal/kube"
	anchormetrics "example.com/anchor/internal/metrics"
	"example.com/anchor/internal/model"
)

type PlacementReconciler struct {
	Core     kubernetes.Interface
	Dynamic  dynamic.Interface
	Strategy anchoraws.PlacementStrategy
	Logger   *slog.Logger
}

func (r *PlacementReconciler) Reconcile(ctx context.Context) error {
	placements, err := r.Dynamic.Resource(anchorkube.PlacementGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list EndpointPlacements: %w", err)
	}
	conflicts := duplicateAddresses(placements.Items)
	for i := range placements.Items {
		key := placements.Items[i].GetNamespace() + "/" + placements.Items[i].GetName()
		if address := conflicts[key]; address != "" {
			_ = r.fail(ctx, &placements.Items[i], fmt.Errorf("address %s is declared by more than one EndpointPlacement", address))
			continue
		}
		if err := r.reconcileOne(ctx, &placements.Items[i]); err != nil {
			r.Logger.Error("placement reconciliation failed", "namespace", placements.Items[i].GetNamespace(), "name", placements.Items[i].GetName(), "error", err)
		}
	}
	return nil
}

func (r *PlacementReconciler) reconcileOne(ctx context.Context, object *unstructured.Unstructured) error {
	var spec model.EndpointPlacementSpec
	rawSpec, _, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil {
		return err
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawSpec, &spec); err != nil {
		return r.fail(ctx, object, fmt.Errorf("decode placement spec: %w", err))
	}
	var oldStatus model.EndpointPlacementStatus
	if rawStatus, found, _ := unstructured.NestedMap(object.Object, "status"); found {
		_ = runtime.DefaultUnstructuredConverter.FromUnstructured(rawStatus, &oldStatus)
	}
	alreadyReady := oldStatus.Phase == model.PlacementReady && oldStatus.ObservedGeneration == object.GetGeneration()
	claim, err := r.Core.ResourceV1().ResourceClaims(object.GetNamespace()).Get(ctx, spec.ClaimName, metav1.GetOptions{})
	if err != nil {
		return r.fail(ctx, object, fmt.Errorf("get ResourceClaim: %w", err))
	}
	if string(claim.UID) != spec.ClaimUID {
		return r.fail(ctx, object, fmt.Errorf("ResourceClaim UID changed"))
	}
	// A standalone claim is briefly unreserved while its old pod is removed and
	// its replacement is being scheduled. Keep the last successful placement as
	// the ownership record during that handoff; clearing it would make Anchor
	// mistake its own ENI assignment for an external conflict.
	if len(claim.Status.ReservedFor) == 0 {
		return nil
	}
	if len(claim.Status.ReservedFor) != 1 {
		return r.fail(ctx, object, fmt.Errorf("claim must have exactly one active pod reservation, found %d", len(claim.Status.ReservedFor)))
	}
	previous := map[string]string{}
	placed := map[string]model.PlacementPath{}
	for _, path := range oldStatus.Paths {
		previous[path.Name] = path.ENIID
		placed[path.Name] = path
	}
	for _, path := range spec.Paths {
		endpoint := anchoraws.Endpoint{
			ClaimUID: spec.ClaimUID, ClaimName: object.GetNamespace() + "/" + spec.ClaimName,
			NodeName: spec.NodeName, Path: path, PreviousENI: previous[path.Name], ForceSteal: spec.ForceSteal,
		}
		if err := r.Strategy.Place(ctx, endpoint); err != nil {
			if alreadyReady {
				anchormetrics.DriftDetections.WithLabelValues(spec.Strategy).Inc()
			}
			return r.failWithPaths(ctx, object, err, placementPaths(placed))
		}
		placed[path.Name] = path
		if err := r.upsertNAD(ctx, object.GetNamespace(), spec.ClaimName, spec.ClaimUID, path); err != nil {
			return r.failWithPaths(ctx, object, err, placementPaths(placed))
		}
	}
	if alreadyReady {
		return nil
	}
	now := metav1.Now()
	status := model.EndpointPlacementStatus{Phase: model.PlacementReady, Message: "all endpoint paths are placed", ObservedGeneration: object.GetGeneration(), PlacedAt: &now, Paths: spec.Paths}
	if err := r.updateStatus(ctx, object, status); err != nil {
		return err
	}
	r.emit(ctx, claim.Namespace, claim.Name, claim.UID, corev1.EventTypeNormal, "EndpointPlaced", fmt.Sprintf("placed %d static endpoint paths on node %s", len(spec.Paths), spec.NodeName))
	return nil
}

func duplicateAddresses(items []unstructured.Unstructured) map[string]string {
	owner := map[string]string{}
	conflicts := map[string]string{}
	for i := range items {
		item := &items[i]
		key := item.GetNamespace() + "/" + item.GetName()
		raw, found, _ := unstructured.NestedMap(item.Object, "spec")
		if !found {
			continue
		}
		var spec model.EndpointPlacementSpec
		if runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &spec) != nil {
			continue
		}
		for _, path := range spec.Paths {
			ip, err := anchoraws.AddressOnly(path.IP)
			if err != nil {
				continue
			}
			if previous := owner[ip]; previous != "" && previous != key {
				conflicts[previous] = ip
				conflicts[key] = ip
			} else {
				owner[ip] = key
			}
		}
	}
	return conflicts
}

func (r *PlacementReconciler) fail(ctx context.Context, object *unstructured.Unstructured, cause error) error {
	return r.failWithPaths(ctx, object, cause, nil)
}

func (r *PlacementReconciler) failWithPaths(ctx context.Context, object *unstructured.Unstructured, cause error, paths []model.PlacementPath) error {
	status := model.EndpointPlacementStatus{Phase: model.PlacementFailed, Message: cause.Error(), ObservedGeneration: object.GetGeneration(), Paths: paths}
	_ = r.updateStatus(ctx, object, status)
	claimName := object.GetName()
	claimUID := object.GetUID()
	if rawSpec, found, _ := unstructured.NestedMap(object.Object, "spec"); found {
		var spec model.EndpointPlacementSpec
		if runtime.DefaultUnstructuredConverter.FromUnstructured(rawSpec, &spec) == nil {
			claimName = spec.ClaimName
			claimUID = types.UID(spec.ClaimUID)
		}
	}
	r.emit(ctx, object.GetNamespace(), claimName, claimUID, corev1.EventTypeWarning, "EndpointPlacementFailed", cause.Error())
	return cause
}

func (r *PlacementReconciler) updateStatus(ctx context.Context, object *unstructured.Unstructured, status model.EndpointPlacementStatus) error {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return err
	}
	copy := object.DeepCopy()
	copy.Object["status"] = raw
	_, err = r.Dynamic.Resource(anchorkube.PlacementGVR).Namespace(object.GetNamespace()).UpdateStatus(ctx, copy, metav1.UpdateOptions{})
	return err
}

func (r *PlacementReconciler) upsertNAD(ctx context.Context, namespace, claimName, claimUID string, path model.PlacementPath) error {
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
	if reflect.DeepEqual(oldSpec, desired.Object["spec"]) {
		return nil
	}
	current.Object["spec"] = desired.Object["spec"]
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
