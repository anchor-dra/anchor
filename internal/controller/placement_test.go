package controller

import (
	"context"
	"errors"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	anchoraws "example.com/anchor/internal/aws"
	anchorkube "example.com/anchor/internal/kube"
	"example.com/anchor/internal/model"
)

type partialStrategy struct {
	calls int
}

func (s *partialStrategy) Validate(context.Context, anchoraws.Endpoint) error { return nil }
func (s *partialStrategy) Release(context.Context, anchoraws.Endpoint) error  { return nil }
func (s *partialStrategy) Place(_ context.Context, _ anchoraws.Endpoint) error {
	s.calls++
	if s.calls == 2 {
		return errors.New("injected second-path failure")
	}
	return nil
}

func TestPartialPlacementRetainsSuccessfulPath(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("11111111-1111-1111-1111-111111111111")
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: "test", UID: uid},
		Status:     resourceapi.ResourceClaimStatus{ReservedFor: []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: "pod", UID: types.UID("pod-uid")}}},
	}
	placement := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "anchor.dra.example.com/v1alpha1", "kind": "EndpointPlacement",
		"metadata": map[string]any{"name": "claim-" + string(uid), "namespace": "test", "generation": int64(1)},
	}}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-a", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{{Name: "a", IP: "10.0.1.10/24", ENIID: "eni-a", Interface: "ens6", SubnetID: "subnet-a"}, {Name: "b", IP: "10.0.2.10/24", ENIID: "eni-b", Interface: "ens7", SubnetID: "subnet-b"}}}
	raw, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	placement.Object["spec"] = raw
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), placement)
	strategy := &partialStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim), Dynamic: dynamicClient, Strategy: strategy}
	if err := reconciler.reconcileOne(ctx, placement); err == nil {
		t.Fatal("expected injected failure")
	}
	updated, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace("test").Get(ctx, placement.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	paths, found, err := unstructured.NestedSlice(updated.Object, "status", "paths")
	if err != nil || !found || len(paths) != 1 {
		t.Fatalf("successful path was not retained: found=%v len=%d err=%v status=%#v", found, len(paths), err, updated.Object["status"])
	}
}

func TestUnreservedClaimRetainsPreviousPlacement(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("22222222-2222-2222-2222-222222222222")
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: "test", UID: uid},
	}
	path := model.PlacementPath{Name: "a", IP: "10.0.1.10/24", ENIID: "eni-old", Interface: "ens6", SubnetID: "subnet-a"}
	placement := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "anchor.dra.example.com/v1alpha1", "kind": "EndpointPlacement",
		"metadata": map[string]any{"name": "claim-" + string(uid), "namespace": "test", "generation": int64(2)},
	}}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-b", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{path}}
	status := model.EndpointPlacementStatus{Phase: model.PlacementReady, ObservedGeneration: 1, Paths: []model.PlacementPath{path}}
	rawSpec, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	rawStatus, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	placement.Object["spec"] = rawSpec
	placement.Object["status"] = rawStatus
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), placement)
	strategy := &partialStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim), Dynamic: dynamicClient, Strategy: strategy}
	if err := reconciler.reconcileOne(ctx, placement); err != nil {
		t.Fatal(err)
	}
	if strategy.calls != 0 {
		t.Fatalf("unreserved handoff invoked placement %d times", strategy.calls)
	}
	updated, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace("test").Get(ctx, placement.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	paths, found, err := unstructured.NestedSlice(updated.Object, "status", "paths")
	if err != nil || !found || len(paths) != 1 {
		t.Fatalf("previous placement was not retained: found=%v len=%d err=%v status=%#v", found, len(paths), err, updated.Object["status"])
	}
}

func TestNADNameIsStableAndBounded(t *testing.T) {
	short := NADName("claim", "a")
	if short != "anchor-claim-a" {
		t.Fatalf("unexpected name %q", short)
	}
	long := NADName("this-is-a-very-long-resource-claim-name-that-needs-to-be-truncated", "long-path")
	if len(long) > 63 || long != NADName("this-is-a-very-long-resource-claim-name-that-needs-to-be-truncated", "long-path") {
		t.Fatalf("invalid deterministic name %q", long)
	}
}
