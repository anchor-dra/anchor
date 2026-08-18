package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	anchoraws "github.com/anchor-dra/anchor/internal/aws"
	anchorkube "github.com/anchor-dra/anchor/internal/kube"
	"github.com/anchor-dra/anchor/internal/model"
)

type partialStrategy struct {
	calls int
}

type recordingStrategy struct {
	endpoints []anchoraws.Endpoint
	err       error
}

func (s *recordingStrategy) Validate(context.Context, anchoraws.Endpoint) error { return nil }
func (s *recordingStrategy) Release(context.Context, anchoraws.Endpoint) error  { return nil }
func (s *recordingStrategy) Place(_ context.Context, endpoint anchoraws.Endpoint) error {
	s.endpoints = append(s.endpoints, endpoint)
	return s.err
}

func testClaim(namespace, name string, uid types.UID) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: uid},
		Status: resourceapi.ResourceClaimStatus{ReservedFor: []resourceapi.ResourceClaimConsumerReference{{
			Resource: "pods", Name: "pod", UID: types.UID("pod-uid-" + string(uid)),
		}}},
	}
}

func testPlacement(t *testing.T, namespace, name string, uid types.UID, spec model.EndpointPlacementSpec, status *model.EndpointPlacementStatus) *unstructured.Unstructured {
	t.Helper()
	rawSpec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	if err != nil {
		t.Fatal(err)
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "dra.anchordra.co/v1alpha1", "kind": "EndpointPlacement",
		"metadata": map[string]any{"name": name, "namespace": namespace, "uid": string(uid), "generation": int64(1)},
		"spec":     rawSpec,
	}}
	if status != nil {
		rawStatus, err := runtime.DefaultUnstructuredConverter.ToUnstructured(status)
		if err != nil {
			t.Fatal(err)
		}
		object.Object["status"] = rawStatus
	}
	return object
}

func testOwnership(t *testing.T, address, namespace, claimName, eni string) *unstructured.Unstructured {
	t.Helper()
	name, err := OwnershipName(address)
	if err != nil {
		t.Fatal(err)
	}
	spec := model.EndpointOwnershipSpec{Address: strings.Split(address, "/")[0], ClaimNamespace: namespace, ClaimName: claimName}
	status := model.EndpointOwnershipStatus{ClaimUID: "old-uid", PathName: "a", ENIID: eni, NodeName: "node-old"}
	rawSpec, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	rawStatus, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "dra.anchordra.co/v1alpha1", "kind": "EndpointOwnership",
		"metadata": map[string]any{"name": name}, "spec": rawSpec, "status": rawStatus,
	}}
}

func testDynamicClient(objects ...runtime.Object) dynamic.Interface {
	listKinds := map[schema.GroupVersionResource]string{
		anchorkube.PlacementGVR: "EndpointPlacementList",
		anchorkube.OwnershipGVR: "EndpointOwnershipList",
		anchorkube.NADGVR:       "NetworkAttachmentDefinitionList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objects...)
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
		"apiVersion": "dra.anchordra.co/v1alpha1", "kind": "EndpointPlacement",
		"metadata": map[string]any{"name": "claim-" + string(uid), "namespace": "test", "generation": int64(1)},
	}}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-a", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{{Name: "a", IP: "10.0.1.10/24", ENIID: "eni-a", Interface: "ens6", SubnetID: "subnet-a"}, {Name: "b", IP: "10.0.2.10/24", ENIID: "eni-b", Interface: "ens7", SubnetID: "subnet-b"}}}
	raw, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	placement.Object["spec"] = raw
	dynamicClient := testDynamicClient(placement)
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
		"apiVersion": "dra.anchordra.co/v1alpha1", "kind": "EndpointPlacement",
		"metadata": map[string]any{"name": "claim-" + string(uid), "namespace": "test", "generation": int64(2)},
	}}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-b", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{path}}
	status := model.EndpointPlacementStatus{Phase: model.PlacementReady, ObservedGeneration: 1, Paths: []model.PlacementPath{path}}
	rawSpec, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	rawStatus, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	placement.Object["spec"] = rawSpec
	placement.Object["status"] = rawStatus
	dynamicClient := testDynamicClient(placement)
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

func TestOwnershipNameIsStableAndCanonical(t *testing.T) {
	first, err := OwnershipName("10.0.1.10/24")
	if err != nil {
		t.Fatal(err)
	}
	second, err := OwnershipName("10.0.1.10/32")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, "ip-") || len(first) != 43 {
		t.Fatalf("unexpected ownership names %q and %q", first, second)
	}
}

func TestRecreatedClaimUsesDurablePreviousENI(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("33333333-3333-3333-3333-333333333333")
	claim := testClaim("test", "endpoint", uid)
	path := model.PlacementPath{Name: "a", IP: "10.0.1.10/24", ENIID: "eni-new", Interface: "ens6", SubnetID: "subnet-a"}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-new", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{path}}
	placement := testPlacement(t, "test", "claim-"+string(uid), uid, spec, nil)
	ownership := testOwnership(t, path.IP, "test", claim.Name, "eni-old")
	dynamicClient := testDynamicClient(placement, ownership)
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim), Dynamic: dynamicClient, Strategy: strategy}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(strategy.endpoints) != 1 || strategy.endpoints[0].PreviousENI != "eni-old" || strategy.endpoints[0].ForceSteal {
		t.Fatalf("unexpected placement request: %#v", strategy.endpoints)
	}
	updated, err := reconciler.getOwnership(ctx, path.IP)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status.ENIID != "eni-new" || updated.Status.ClaimUID != string(uid) || !sameLogicalOwner(updated.Spec, "test", "endpoint") {
		t.Fatalf("ownership was not updated: %#v", updated)
	}
	storedPlacement, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace("test").Get(ctx, placement.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owners := storedPlacement.GetOwnerReferences()
	if len(owners) != 1 || owners[0].UID != uid || owners[0].Kind != "ResourceClaim" {
		t.Fatalf("placement owner reference is wrong: %#v", owners)
	}
	nad, err := dynamicClient.Resource(anchorkube.NADGVR).Namespace("test").Get(ctx, NADName("endpoint", "a"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owners = nad.GetOwnerReferences()
	if len(owners) != 1 || owners[0].UID != placement.GetUID() || owners[0].Kind != "EndpointPlacement" {
		t.Fatalf("NAD owner reference is wrong: %#v", owners)
	}
}

func TestStaleClaimMigrationDoesNotConflictWithRecreation(t *testing.T) {
	ctx := context.Background()
	oldUID := types.UID("44444444-4444-4444-4444-444444444444")
	newUID := types.UID("55555555-5555-5555-5555-555555555555")
	claim := testClaim("recreated", "endpoint", newUID)
	oldPath := model.PlacementPath{Name: "a", IP: "10.0.1.20/24", ENIID: "eni-old", Interface: "ens6", SubnetID: "subnet-a"}
	newPath := oldPath
	newPath.ENIID = "eni-new"
	oldSpec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(oldUID), NodeName: "node-old", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{oldPath}}
	newSpec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(newUID), NodeName: "node-new", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{newPath}}
	oldStatus := &model.EndpointPlacementStatus{Phase: model.PlacementReady, ObservedGeneration: 1, Paths: []model.PlacementPath{oldPath}}
	oldPlacement := testPlacement(t, "recreated", "claim-"+string(oldUID), oldUID, oldSpec, oldStatus)
	newPlacement := testPlacement(t, "recreated", "claim-"+string(newUID), newUID, newSpec, nil)
	oldNAD := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "k8s.cni.cncf.io/v1", "kind": "NetworkAttachmentDefinition",
		"metadata": map[string]any{"name": NADName(claim.Name, "a"), "namespace": "recreated", "labels": map[string]any{"dra.anchordra.co/claim-uid": string(oldUID)}},
		"spec":     map[string]any{"config": "old"},
	}}
	dynamicClient := testDynamicClient(oldPlacement, newPlacement, oldNAD)
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim), Dynamic: dynamicClient, Strategy: strategy}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(strategy.endpoints) != 1 || strategy.endpoints[0].PreviousENI != "eni-old" {
		t.Fatalf("recreated claim did not inherit ownership: %#v", strategy.endpoints)
	}
	if _, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace("recreated").Get(ctx, oldPlacement.GetName(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stale placement still exists: %v", err)
	}
	ownership, err := reconciler.getOwnership(ctx, newPath.IP)
	if err != nil {
		t.Fatal(err)
	}
	if ownership.Status.ENIID != "eni-new" || ownership.Status.ClaimUID != string(newUID) {
		t.Fatalf("ownership did not move to recreated claim: %#v", ownership.Status)
	}
	nad, err := dynamicClient.Resource(anchorkube.NADGVR).Namespace("recreated").Get(ctx, NADName(claim.Name, "a"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if nad.GetLabels()["dra.anchordra.co/claim-uid"] != string(newUID) {
		t.Fatalf("NAD was not recreated for the new claim: %#v", nad.GetLabels())
	}
}

func TestEstablishedOwnerWinsLiveDuplicate(t *testing.T) {
	ctx := context.Background()
	address := "10.0.1.30/24"
	ownerUID := types.UID("66666666-6666-6666-6666-666666666666")
	otherUID := types.UID("77777777-7777-7777-7777-777777777777")
	ownerClaim := testClaim("owner", "endpoint", ownerUID)
	otherClaim := testClaim("other", "endpoint", otherUID)
	ownerPath := model.PlacementPath{Name: "a", IP: address, ENIID: "eni-owner", Interface: "ens6", SubnetID: "subnet-a"}
	otherPath := ownerPath
	otherPath.ENIID = "eni-other"
	ownerSpec := model.EndpointPlacementSpec{ClaimName: ownerClaim.Name, ClaimUID: string(ownerUID), NodeName: "node-owner", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{ownerPath}}
	otherSpec := model.EndpointPlacementSpec{ClaimName: otherClaim.Name, ClaimUID: string(otherUID), NodeName: "node-other", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{otherPath}}
	ownerPlacement := testPlacement(t, "owner", "claim-"+string(ownerUID), ownerUID, ownerSpec, nil)
	otherPlacement := testPlacement(t, "other", "claim-"+string(otherUID), otherUID, otherSpec, nil)
	ownership := testOwnership(t, address, "owner", "endpoint", "eni-old")
	dynamicClient := testDynamicClient(ownerPlacement, otherPlacement, ownership)
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(ownerClaim, otherClaim), Dynamic: dynamicClient, Strategy: strategy}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(strategy.endpoints) != 1 || strategy.endpoints[0].ClaimName != "owner/endpoint" {
		t.Fatalf("established owner was not the only placement: %#v", strategy.endpoints)
	}
	failed, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace("other").Get(ctx, otherPlacement.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	phase, _, _ := unstructured.NestedString(failed.Object, "status", "phase")
	message, _, _ := unstructured.NestedString(failed.Object, "status", "message")
	if phase != model.PlacementFailed || !strings.Contains(message, "established owner is owner/endpoint") {
		t.Fatalf("competitor was not rejected correctly: phase=%s message=%q", phase, message)
	}
}

func TestAmbiguousLiveDuplicateFailsWithoutCreatingOwnership(t *testing.T) {
	ctx := context.Background()
	address := "10.0.1.40/24"
	claims := []*resourceapi.ResourceClaim{
		testClaim("a", "endpoint", types.UID("88888888-8888-8888-8888-888888888888")),
		testClaim("b", "endpoint", types.UID("99999999-9999-9999-9999-999999999999")),
	}
	objects := []runtime.Object{}
	for _, claim := range claims {
		path := model.PlacementPath{Name: "a", IP: address, ENIID: "eni-" + claim.Namespace, Interface: "ens6", SubnetID: "subnet-a"}
		spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(claim.UID), NodeName: "node-" + claim.Namespace, Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{path}}
		objects = append(objects, testPlacement(t, claim.Namespace, "claim-"+string(claim.UID), claim.UID, spec, nil))
	}
	dynamicClient := testDynamicClient(objects...)
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claims[0], claims[1]), Dynamic: dynamicClient, Strategy: strategy}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(strategy.endpoints) != 0 {
		t.Fatalf("ambiguous claims caused a placement: %#v", strategy.endpoints)
	}
	if _, err := reconciler.getOwnership(ctx, address); !apierrors.IsNotFound(err) {
		t.Fatalf("ambiguous claims created ownership: %v", err)
	}
}

func TestForceStealTransfersDurableOwnership(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	claim := testClaim("new", "endpoint", uid)
	path := model.PlacementPath{Name: "a", IP: "10.0.1.50/24", ENIID: "eni-new", Interface: "ens6", SubnetID: "subnet-a"}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-new", Strategy: model.StrategyIPReassign, ForceSteal: true, Paths: []model.PlacementPath{path}}
	placement := testPlacement(t, "new", "claim-"+string(uid), uid, spec, nil)
	ownership := testOwnership(t, path.IP, "old", "endpoint", "eni-old")
	dynamicClient := testDynamicClient(placement, ownership)
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim), Dynamic: dynamicClient, Strategy: strategy}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(strategy.endpoints) != 1 || !strategy.endpoints[0].ForceSteal {
		t.Fatalf("force-steal was not passed to the strategy: %#v", strategy.endpoints)
	}
	updated, err := reconciler.getOwnership(ctx, path.IP)
	if err != nil {
		t.Fatal(err)
	}
	if !sameLogicalOwner(updated.Spec, "new", "endpoint") || updated.Status.ENIID != "eni-new" {
		t.Fatalf("ownership was not transferred: %#v", updated)
	}
}

func TestFailedForceStealDoesNotTransferDurableOwnership(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	claim := testClaim("new", "endpoint", uid)
	path := model.PlacementPath{Name: "a", IP: "10.0.1.60/24", ENIID: "eni-new", Interface: "ens6", SubnetID: "subnet-a"}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-new", Strategy: model.StrategyIPReassign, ForceSteal: true, Paths: []model.PlacementPath{path}}
	placement := testPlacement(t, "new", "claim-"+string(uid), uid, spec, nil)
	ownership := testOwnership(t, path.IP, "old", "endpoint", "eni-old")
	dynamicClient := testDynamicClient(placement, ownership)
	strategy := &recordingStrategy{err: errors.New("injected placement failure")}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim), Dynamic: dynamicClient, Strategy: strategy}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	updated, err := reconciler.getOwnership(ctx, path.IP)
	if err != nil {
		t.Fatal(err)
	}
	if !sameLogicalOwner(updated.Spec, "old", "endpoint") || updated.Status.ENIID != "eni-old" {
		t.Fatalf("failed placement transferred ownership: %#v", updated)
	}
}

func TestNADOwnerReferenceIsRepairedWithoutSpecChange(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("cccccccc-cccc-cccc-cccc-cccccccccccc")
	placement := testPlacement(t, "test", "claim-"+string(uid), uid, model.EndpointPlacementSpec{}, nil)
	path := model.PlacementPath{Name: "a", IP: "10.0.1.70/24", ENIID: "eni-a", Interface: "ens6", SubnetID: "subnet-a"}
	dynamicClient := testDynamicClient(placement)
	reconciler := &PlacementReconciler{Dynamic: dynamicClient}

	if err := reconciler.upsertNAD(ctx, placement, "endpoint", string(uid), path); err != nil {
		t.Fatal(err)
	}
	name := NADName("endpoint", "a")
	nad, err := dynamicClient.Resource(anchorkube.NADGVR).Namespace("test").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nad.SetOwnerReferences(nil)
	if _, err := dynamicClient.Resource(anchorkube.NADGVR).Namespace("test").Update(ctx, nad, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.upsertNAD(ctx, placement, "endpoint", string(uid), path); err != nil {
		t.Fatal(err)
	}
	repaired, err := dynamicClient.Resource(anchorkube.NADGVR).Namespace("test").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owners := repaired.GetOwnerReferences()
	if len(owners) != 1 || owners[0].UID != uid || owners[0].Kind != "EndpointPlacement" {
		t.Fatalf("NAD owner reference was not repaired: %#v", owners)
	}
}

func TestAmbiguousLegacyOwnershipFailsWithoutGuessing(t *testing.T) {
	ctx := context.Background()
	address := "10.0.1.80/24"
	objects := []runtime.Object{}
	claims := []runtime.Object{}
	for i, namespace := range []string{"first", "second"} {
		uid := types.UID(fmt.Sprintf("dddddddd-dddd-dddd-dddd-dddddddddd%02d", i))
		claim := testClaim(namespace, "endpoint", uid)
		claims = append(claims, claim)
		path := model.PlacementPath{Name: "a", IP: address, ENIID: "eni-" + namespace, Interface: "ens6", SubnetID: "subnet-a"}
		spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-" + namespace, Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{path}}
		status := &model.EndpointPlacementStatus{Phase: model.PlacementReady, ObservedGeneration: 1, Paths: []model.PlacementPath{path}}
		objects = append(objects, testPlacement(t, namespace, "claim-"+string(uid), uid, spec, status))
	}
	dynamicClient := testDynamicClient(objects...)
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claims...), Dynamic: dynamicClient, Strategy: &recordingStrategy{}}

	err := reconciler.Reconcile(ctx)
	if err == nil || !strings.Contains(err.Error(), "ambiguous legacy owners") {
		t.Fatalf("expected ambiguous migration failure, got %v", err)
	}
	if _, err := reconciler.getOwnership(ctx, address); !apierrors.IsNotFound(err) {
		t.Fatalf("ambiguous migration created ownership: %v", err)
	}
}
