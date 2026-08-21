package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	anchoraws "github.com/anchor-dra/anchor/internal/aws"
	"github.com/anchor-dra/anchor/internal/constants"
	anchorkube "github.com/anchor-dra/anchor/internal/kube"
	"github.com/anchor-dra/anchor/internal/model"
)

type ownershipRecord struct {
	Object *unstructured.Unstructured
	Spec   model.EndpointOwnershipSpec
	Status model.EndpointOwnershipStatus
}

// OwnershipName returns the stable cluster-scoped record name for an IPv4 address.
func OwnershipName(address string) (string, error) {
	ip, err := anchoraws.AddressOnly(address)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(ip))
	return fmt.Sprintf("ip-%x", sum[:20]), nil
}

func ownershipAddress(path model.PlacementPath) (string, error) {
	return anchoraws.AddressOnly(path.IP)
}

func sameLogicalOwner(spec model.EndpointOwnershipSpec, namespace, name string) bool {
	return spec.ClaimNamespace == namespace && spec.ClaimName == name
}

func ownershipClaimName(spec model.EndpointPlacementSpec) string {
	if spec.OwnershipName != "" {
		return spec.OwnershipName
	}
	return spec.ClaimName
}

func decodeOwnership(object *unstructured.Unstructured) (*ownershipRecord, error) {
	record := &ownershipRecord{Object: object}
	rawSpec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("EndpointOwnership %s has no spec", object.GetName())
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawSpec, &record.Spec); err != nil {
		return nil, fmt.Errorf("decode EndpointOwnership %s spec: %w", object.GetName(), err)
	}
	if rawStatus, found, _ := unstructured.NestedMap(object.Object, "status"); found {
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawStatus, &record.Status); err != nil {
			return nil, fmt.Errorf("decode EndpointOwnership %s status: %w", object.GetName(), err)
		}
	}
	return record, nil
}

func (r *PlacementReconciler) listOwnerships(ctx context.Context) (map[string]*ownershipRecord, error) {
	items, err := r.Dynamic.Resource(anchorkube.OwnershipGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list EndpointOwnerships: %w", err)
	}
	result := make(map[string]*ownershipRecord, len(items.Items))
	for i := range items.Items {
		record, err := decodeOwnership(&items.Items[i])
		if err != nil {
			return nil, err
		}
		ip, err := anchoraws.AddressOnly(record.Spec.Address + "/32")
		if err != nil {
			return nil, fmt.Errorf("EndpointOwnership %s: %w", record.Object.GetName(), err)
		}
		if previous := result[ip]; previous != nil {
			return nil, fmt.Errorf("address %s has duplicate EndpointOwnership records %s and %s", ip, previous.Object.GetName(), record.Object.GetName())
		}
		result[ip] = record
	}
	return result, nil
}

func (r *PlacementReconciler) getOwnership(ctx context.Context, address string) (*ownershipRecord, error) {
	name, err := OwnershipName(address)
	if err != nil {
		return nil, err
	}
	object, err := r.Dynamic.Resource(anchorkube.OwnershipGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return decodeOwnership(object)
}

func (r *PlacementReconciler) createOwnership(ctx context.Context, address, namespace, claimName string) (*ownershipRecord, error) {
	name, err := OwnershipName(address)
	if err != nil {
		return nil, err
	}
	ip, err := anchoraws.AddressOnly(address)
	if err != nil {
		return nil, err
	}
	spec := model.EndpointOwnershipSpec{Address: ip, ClaimNamespace: namespace, ClaimName: claimName}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	if err != nil {
		return nil, err
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.APIGroup + "/" + constants.APIVersion,
		"kind":       "EndpointOwnership",
		"metadata":   map[string]any{"name": name},
		"spec":       raw,
	}}
	created, err := r.Dynamic.Resource(anchorkube.OwnershipGVR).Create(ctx, object, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return r.getOwnership(ctx, address)
	}
	if err != nil {
		return nil, err
	}
	return decodeOwnership(created)
}

func (r *PlacementReconciler) reserveOwnerships(ctx context.Context, namespace string, spec model.EndpointPlacementSpec) (map[string]*ownershipRecord, error) {
	claimName := ownershipClaimName(spec)
	records := make(map[string]*ownershipRecord, len(spec.Paths))
	missing := make([]model.PlacementPath, 0, len(spec.Paths))
	for _, path := range spec.Paths {
		ip, err := ownershipAddress(path)
		if err != nil {
			return nil, err
		}
		record, err := r.getOwnership(ctx, path.IP)
		if apierrors.IsNotFound(err) {
			missing = append(missing, path)
			continue
		}
		if err != nil {
			return nil, err
		}
		if !sameLogicalOwner(record.Spec, namespace, claimName) && !spec.ForceSteal {
			return nil, fmt.Errorf("address %s is owned by ResourceClaim %s/%s; set force-steal explicitly to transfer ownership", ip, record.Spec.ClaimNamespace, record.Spec.ClaimName)
		}
		records[ip] = record
	}
	for _, path := range missing {
		ip, _ := ownershipAddress(path)
		record, err := r.createOwnership(ctx, path.IP, namespace, claimName)
		if err != nil {
			return nil, err
		}
		if !sameLogicalOwner(record.Spec, namespace, claimName) && !spec.ForceSteal {
			return nil, fmt.Errorf("address %s is owned by ResourceClaim %s/%s; set force-steal explicitly to transfer ownership", ip, record.Spec.ClaimNamespace, record.Spec.ClaimName)
		}
		records[ip] = record
	}
	return records, nil
}

func (r *PlacementReconciler) markOwnershipPlaced(ctx context.Context, record *ownershipRecord, namespace string, spec model.EndpointPlacementSpec, path model.PlacementPath) error {
	claimName := ownershipClaimName(spec)
	ownerChanged := false
	if !sameLogicalOwner(record.Spec, namespace, claimName) {
		ownerChanged = true
		record.Spec.ClaimNamespace = namespace
		record.Spec.ClaimName = claimName
		rawSpec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&record.Spec)
		if err != nil {
			return err
		}
		copy := record.Object.DeepCopy()
		copy.Object["spec"] = rawSpec
		updated, err := r.Dynamic.Resource(anchorkube.OwnershipGVR).Update(ctx, copy, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		record.Object = updated
	}
	if !ownerChanged && record.Status.ClaimUID == spec.ClaimUID && record.Status.PathName == path.Name && record.Status.ENIID == path.ENIID && record.Status.NodeName == spec.NodeName {
		return nil
	}
	now := metav1.Now()
	record.Status = model.EndpointOwnershipStatus{
		ClaimUID: spec.ClaimUID, PathName: path.Name, ENIID: path.ENIID,
		NodeName: spec.NodeName, LastPlacedAt: &now,
	}
	rawStatus, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&record.Status)
	if err != nil {
		return err
	}
	copy := record.Object.DeepCopy()
	copy.Object["status"] = rawStatus
	updated, err := r.Dynamic.Resource(anchorkube.OwnershipGVR).UpdateStatus(ctx, copy, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	record.Object = updated
	return nil
}

type ownershipEvidence struct {
	namespace string
	spec      model.EndpointPlacementSpec
	path      model.PlacementPath
	placedAt  time.Time
}

func (r *PlacementReconciler) migrateOwnerships(ctx context.Context, placements []livePlacement) error {
	byAddress := map[string][]ownershipEvidence{}
	for i := range placements {
		placement := &placements[i]
		if placement.status.Phase != model.PlacementReady ||
			placement.status.ObservedGeneration != placement.object.GetGeneration() ||
			!reflect.DeepEqual(placement.status.Paths, placement.spec.Paths) {
			continue
		}
		placedAt := time.Time{}
		if placement.status.PlacedAt != nil {
			placedAt = placement.status.PlacedAt.Time
		}
		for _, path := range placement.status.Paths {
			if path.ENIID == "" {
				continue
			}
			ip, err := ownershipAddress(path)
			if err != nil {
				return err
			}
			byAddress[ip] = append(byAddress[ip], ownershipEvidence{namespace: placement.object.GetNamespace(), spec: placement.spec, path: path, placedAt: placedAt})
		}
	}

	for address, evidence := range byAddress {
		record, err := r.getOwnership(ctx, evidence[0].path.IP)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err == nil && record.Status.ENIID != "" {
			continue
		}

		selected := evidence[0]
		for _, candidate := range evidence[1:] {
			if candidate.namespace != selected.namespace || ownershipClaimName(candidate.spec) != ownershipClaimName(selected.spec) {
				return fmt.Errorf("address %s has ambiguous legacy owners %s/%s and %s/%s", address, selected.namespace, selected.spec.ClaimName, candidate.namespace, candidate.spec.ClaimName)
			}
			if candidate.path.ENIID == selected.path.ENIID {
				continue
			}
			if candidate.placedAt.Equal(selected.placedAt) {
				return fmt.Errorf("address %s has ambiguous legacy ENIs %s and %s for %s/%s", address, selected.path.ENIID, candidate.path.ENIID, selected.namespace, selected.spec.ClaimName)
			}
			if selected.placedAt.IsZero() || (!candidate.placedAt.IsZero() && candidate.placedAt.After(selected.placedAt)) {
				selected = candidate
			}
		}

		if err == nil {
			if !sameLogicalOwner(record.Spec, selected.namespace, ownershipClaimName(selected.spec)) {
				continue
			}
		} else {
			record, err = r.createOwnership(ctx, selected.path.IP, selected.namespace, ownershipClaimName(selected.spec))
			if err != nil {
				return err
			}
			if !sameLogicalOwner(record.Spec, selected.namespace, ownershipClaimName(selected.spec)) {
				return fmt.Errorf("cannot migrate address %s: ownership already belongs to %s/%s", address, record.Spec.ClaimNamespace, record.Spec.ClaimName)
			}
		}
		if err := r.markOwnershipPlaced(ctx, record, selected.namespace, selected.spec, selected.path); err != nil {
			return err
		}
	}
	return nil
}
