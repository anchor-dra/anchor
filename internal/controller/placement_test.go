package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	anchoraws "github.com/anchor-dra/anchor/internal/aws"
	"github.com/anchor-dra/anchor/internal/constants"
	anchorkube "github.com/anchor-dra/anchor/internal/kube"
	"github.com/anchor-dra/anchor/internal/model"
)

type partialStrategy struct {
	calls int
}

type recordingStrategy struct {
	endpoints        []anchoraws.Endpoint
	verified         []anchoraws.Endpoint
	verifyCalls      int
	err              error
	verification     []anchoraws.VerificationResult
	verifyErr        error
	placementResults []anchoraws.PlacementResult
}

func (s *recordingStrategy) Validate(context.Context, anchoraws.Endpoint) error { return nil }
func (s *recordingStrategy) Release(context.Context, anchoraws.Endpoint) error  { return nil }
func (s *recordingStrategy) Place(_ context.Context, endpoint anchoraws.Endpoint) error {
	s.endpoints = append(s.endpoints, endpoint)
	return s.err
}
func (s *recordingStrategy) PlaceBatch(ctx context.Context, endpoints []anchoraws.Endpoint) []anchoraws.PlacementResult {
	if s.placementResults != nil {
		s.endpoints = append(s.endpoints, endpoints...)
		return s.placementResults
	}
	results := make([]anchoraws.PlacementResult, len(endpoints))
	for i := range endpoints {
		results[i].Err = s.Place(ctx, endpoints[i])
	}
	return results
}
func (s *recordingStrategy) VerifyBatch(_ context.Context, endpoints []anchoraws.Endpoint) ([]anchoraws.VerificationResult, error) {
	s.verifyCalls++
	s.verified = append(s.verified, endpoints...)
	if s.verification != nil || s.verifyErr != nil {
		return s.verification, s.verifyErr
	}
	results := make([]anchoraws.VerificationResult, len(endpoints))
	for i := range results {
		results[i].Placed = true
	}
	return results, nil
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

func authorizePlacement(t *testing.T, claim *resourceapi.ResourceClaim, spec *model.EndpointPlacementSpec) (*corev1.Pod, *unstructured.Unstructured) {
	t.Helper()
	profile := "test-profile"
	spec.RequestName = "endpoint"
	spec.PoolName = spec.NodeName
	spec.DeviceName = profile + "-slot-0"
	if spec.Strategy == "" {
		spec.Strategy = model.StrategyIPReassign
	}
	classPaths := make([]model.PathSpec, 0, len(spec.Paths))
	addresses := make([]model.AddressSpec, 0, len(spec.Paths))
	inventorySpecPaths := make([]model.ENIPath, 0, len(spec.Paths))
	inventoryStatusPaths := make([]model.ENIPath, 0, len(spec.Paths))
	for index := range spec.Paths {
		path := &spec.Paths[index]
		if path.InterfaceName == "" {
			path.InterfaceName = fmt.Sprintf("carrier-%d", index)
		}
		if path.RoutingTable == 0 {
			path.RoutingTable = 100 + index
		}
		classPath := model.PathSpec{Name: path.Name, InterfaceName: path.InterfaceName, RoutingTable: path.RoutingTable, SubnetID: path.SubnetID, ENITagSelector: model.TagSelector{"test": "true"}, Gateway: path.Gateway, Routes: append([]string(nil), path.Routes...), RouteTableIDs: append([]string(nil), path.RouteTableIDs...)}
		if spec.Strategy == model.StrategyRouteRepoint {
			classPath.Subnets = []model.AWSSubnetSpec{{SubnetID: path.SubnetID, Gateway: path.Gateway}}
			classPath.SubnetID = ""
			classPath.Gateway = ""
		}
		classPaths = append(classPaths, classPath)
		addresses = append(addresses, model.AddressSpec{Path: path.Name, IP: path.IP})
		inventoryPath := model.ENIPath{Name: path.Name, ENIID: path.ENIID, MAC: path.ParentMAC, SubnetID: path.SubnetID, SubnetCIDR: path.SubnetCIDR, Tags: map[string]string{"test": "true"}}
		inventorySpecPaths = append(inventorySpecPaths, inventoryPath)
		inventoryPath.Interface = path.Interface
		inventoryStatusPaths = append(inventoryStatusPaths, inventoryPath)
	}
	classRaw, err := json.Marshal(model.DeviceClassParameters{Strategy: spec.Strategy, Profile: profile, SlotsPerNode: 1, Paths: classPaths})
	if err != nil {
		t.Fatal(err)
	}
	claimRaw, err := json.Marshal(model.ClaimParameters{Addresses: addresses})
	if err != nil {
		t.Fatal(err)
	}
	claim.Status.Allocation = &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{
		Results: []resourceapi.DeviceRequestAllocationResult{{Request: spec.RequestName, Driver: constants.DriverName, Pool: spec.PoolName, Device: spec.DeviceName}},
		Config: []resourceapi.DeviceAllocationConfiguration{
			{Source: resourceapi.AllocationConfigSourceClass, DeviceConfiguration: resourceapi.DeviceConfiguration{Opaque: &resourceapi.OpaqueDeviceConfiguration{Driver: constants.DriverName, Parameters: runtime.RawExtension{Raw: classRaw}}}},
			{Source: resourceapi.AllocationConfigSourceClaim, DeviceConfiguration: resourceapi.DeviceConfiguration{Opaque: &resourceapi.OpaqueDeviceConfiguration{Driver: constants.DriverName, Parameters: runtime.RawExtension{Raw: claimRaw}}}},
		},
	}}
	if spec.ForceSteal {
		if claim.Annotations == nil {
			claim.Annotations = map[string]string{}
		}
		claim.Annotations[constants.ForceStealAnnotation] = "true"
	}
	reservation := claim.Status.ReservedFor[0]
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: reservation.Name, Namespace: claim.Namespace, UID: reservation.UID}, Spec: corev1.PodSpec{NodeName: spec.NodeName}}
	inventorySpec := model.AnchorNodeInventorySpec{NodeName: spec.NodeName, Profiles: map[string][]model.ENIPath{profile: inventorySpecPaths}, SlotsPerNode: map[string]int{profile: 1}}
	inventoryStatus := model.AnchorNodeInventoryStatus{Ready: true, ObservedGeneration: 1, Interfaces: map[string][]model.ENIPath{profile: inventoryStatusPaths}}
	rawSpec, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&inventorySpec)
	rawStatus, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&inventoryStatus)
	inventory := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.APIGroup + "/" + constants.APIVersion, "kind": "AnchorNodeInventory",
		"metadata": map[string]any{"name": spec.NodeName, "generation": int64(1)}, "spec": rawSpec, "status": rawStatus,
	}}
	return pod, inventory
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

func testDynamicClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	listKinds := map[schema.GroupVersionResource]string{
		anchorkube.InventoryGVR: "AnchorNodeInventoryList",
		anchorkube.PlacementGVR: "EndpointPlacementList",
		anchorkube.OwnershipGVR: "EndpointOwnershipList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objects...)
}

func countActions(actions []ktesting.Action, verb, resource, subresource string) int {
	count := 0
	for _, action := range actions {
		if action.GetVerb() == verb && action.GetResource().Resource == resource && action.GetSubresource() == subresource {
			count++
		}
	}
	return count
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
func (s *partialStrategy) PlaceBatch(ctx context.Context, endpoints []anchoraws.Endpoint) []anchoraws.PlacementResult {
	results := make([]anchoraws.PlacementResult, len(endpoints))
	for i := range endpoints {
		results[i].Err = s.Place(ctx, endpoints[i])
	}
	return results
}
func (s *partialStrategy) VerifyBatch(_ context.Context, endpoints []anchoraws.Endpoint) ([]anchoraws.VerificationResult, error) {
	results := make([]anchoraws.VerificationResult, len(endpoints))
	for i := range results {
		results[i].Placed = true
	}
	return results, nil
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
	pod, inventory := authorizePlacement(t, claim, &spec)
	raw, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	placement.Object["spec"] = raw
	dynamicClient := testDynamicClient(placement, inventory)
	strategy := &partialStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim, pod), Dynamic: dynamicClient, Strategy: strategy}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
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
	if err := reconciler.Reconcile(ctx); err != nil {
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
	pod, inventory := authorizePlacement(t, claim, &spec)
	placement := testPlacement(t, "test", "claim-"+string(uid), uid, spec, nil)
	ownership := testOwnership(t, path.IP, "test", claim.Name, "eni-old")
	dynamicClient := testDynamicClient(placement, ownership, inventory)
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim, pod), Dynamic: dynamicClient, Strategy: strategy}

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
}

func TestRecreatedTemplateClaimUsesStableLogicalOwnership(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("34343434-3434-3434-3434-343434343434")
	claim := testClaim("test", "workload-endpoint-new12", uid)
	path := model.PlacementPath{Name: "a", IP: "10.0.1.12/24", ENIID: "eni-new", Interface: "ens6", SubnetID: "subnet-a"}
	spec := model.EndpointPlacementSpec{
		ClaimName: claim.Name, ClaimUID: string(uid), OwnershipName: "workload-endpoint",
		NodeName: "node-new", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{path},
	}
	pod, inventory := authorizePlacement(t, claim, &spec)
	controller := true
	claim.Annotations = map[string]string{"resource.kubernetes.io/pod-claim-name": "endpoint"}
	claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID, Controller: &controller}}
	pod.Name = "workload"
	claim.Status.ReservedFor[0].Name = pod.Name
	claim.Status.ReservedFor[0].UID = pod.UID
	claim.OwnerReferences[0].Name = pod.Name
	placement := testPlacement(t, claim.Namespace, "claim-"+string(uid), uid, spec, nil)
	ownership := testOwnership(t, path.IP, claim.Namespace, spec.OwnershipName, "eni-old")
	dynamicClient := testDynamicClient(placement, ownership, inventory)
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim, pod), Dynamic: dynamicClient, Strategy: strategy}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(strategy.endpoints) != 1 || strategy.endpoints[0].PreviousENI != "eni-old" || strategy.endpoints[0].ForceSteal {
		t.Fatalf("template claim did not inherit logical ownership: %#v", strategy.endpoints)
	}
	updated, err := reconciler.getOwnership(ctx, path.IP)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Spec.ClaimName != spec.OwnershipName || updated.Status.ClaimUID != string(uid) {
		t.Fatalf("template ownership was not advanced: %#v", updated)
	}
}

func TestStaleClaimCleanupDoesNotConflictWithRecreation(t *testing.T) {
	ctx := context.Background()
	oldUID := types.UID("44444444-4444-4444-4444-444444444444")
	newUID := types.UID("55555555-5555-5555-5555-555555555555")
	claim := testClaim("recreated", "endpoint", newUID)
	oldPath := model.PlacementPath{Name: "a", IP: "10.0.1.20/24", ENIID: "eni-old", Interface: "ens6", SubnetID: "subnet-a"}
	newPath := oldPath
	newPath.ENIID = "eni-new"
	oldSpec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(oldUID), NodeName: "node-old", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{oldPath}}
	newSpec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(newUID), NodeName: "node-new", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{newPath}}
	pod, inventory := authorizePlacement(t, claim, &newSpec)
	oldStatus := &model.EndpointPlacementStatus{Phase: model.PlacementReady, ObservedGeneration: 1, Paths: []model.PlacementPath{oldPath}}
	oldPlacement := testPlacement(t, "recreated", "claim-"+string(oldUID), oldUID, oldSpec, oldStatus)
	newPlacement := testPlacement(t, "recreated", "claim-"+string(newUID), newUID, newSpec, nil)
	ownership := testOwnership(t, oldPath.IP, claim.Namespace, claim.Name, "eni-old")
	dynamicClient := testDynamicClient(oldPlacement, newPlacement, ownership, inventory)
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim, pod), Dynamic: dynamicClient, Strategy: strategy}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(strategy.endpoints) != 1 || strategy.endpoints[0].PreviousENI != "eni-old" {
		t.Fatalf("recreated claim did not inherit ownership: %#v", strategy.endpoints)
	}
	if _, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace("recreated").Get(ctx, oldPlacement.GetName(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stale placement still exists: %v", err)
	}
	storedOwnership, err := reconciler.getOwnership(ctx, newPath.IP)
	if err != nil {
		t.Fatal(err)
	}
	if storedOwnership.Status.ENIID != "eni-new" || storedOwnership.Status.ClaimUID != string(newUID) {
		t.Fatalf("ownership did not move to recreated claim: %#v", storedOwnership.Status)
	}
}

func TestDeletingClaimRetainsPlacementUntilGone(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("56565656-5656-5656-5656-565656565656")
	now := metav1.Now()
	claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "endpoint", Namespace: "test", UID: uid, DeletionTimestamp: &now,
		Finalizers: []string{"test.anchordra.co/hold"},
	}}
	path := model.PlacementPath{Name: "a", IP: "10.0.1.25/24", ENIID: "eni-old", Interface: "ens6", SubnetID: "subnet-a"}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-old", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{path}}
	status := &model.EndpointPlacementStatus{Phase: model.PlacementReady, ObservedGeneration: 1, Paths: []model.PlacementPath{path}}
	placement := testPlacement(t, claim.Namespace, "claim-"+string(uid), uid, spec, status)
	dynamicClient := testDynamicClient(placement)
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim), Dynamic: dynamicClient, Strategy: &recordingStrategy{}}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace(claim.Namespace).Get(ctx, placement.GetName(), metav1.GetOptions{}); err != nil {
		t.Fatalf("terminating claim lost its placement: %v", err)
	}
	reconciler.Core = fake.NewSimpleClientset()
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace(claim.Namespace).Get(ctx, placement.GetName(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("gone claim retained its placement: %v", err)
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
	ownerPod, ownerInventory := authorizePlacement(t, ownerClaim, &ownerSpec)
	otherPod, otherInventory := authorizePlacement(t, otherClaim, &otherSpec)
	ownerPlacement := testPlacement(t, "owner", "claim-"+string(ownerUID), ownerUID, ownerSpec, nil)
	otherPlacement := testPlacement(t, "other", "claim-"+string(otherUID), otherUID, otherSpec, nil)
	ownership := testOwnership(t, address, "owner", "endpoint", "eni-old")
	dynamicClient := testDynamicClient(ownerPlacement, otherPlacement, ownership, ownerInventory, otherInventory)
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(ownerClaim, otherClaim, ownerPod, otherPod), Dynamic: dynamicClient, Strategy: strategy}

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
	coreObjects := []runtime.Object{claims[0], claims[1]}
	for _, claim := range claims {
		path := model.PlacementPath{Name: "a", IP: address, ENIID: "eni-" + claim.Namespace, Interface: "ens6", SubnetID: "subnet-a"}
		spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(claim.UID), NodeName: "node-" + claim.Namespace, Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{path}}
		pod, inventory := authorizePlacement(t, claim, &spec)
		objects = append(objects, testPlacement(t, claim.Namespace, "claim-"+string(claim.UID), claim.UID, spec, nil))
		objects = append(objects, inventory)
		coreObjects = append(coreObjects, pod)
	}
	dynamicClient := testDynamicClient(objects...)
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(coreObjects...), Dynamic: dynamicClient, Strategy: strategy}

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
	pod, inventory := authorizePlacement(t, claim, &spec)
	placement := testPlacement(t, "new", "claim-"+string(uid), uid, spec, nil)
	ownership := testOwnership(t, path.IP, "old", "endpoint", "eni-old")
	dynamicClient := testDynamicClient(placement, ownership, inventory)
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim, pod), Dynamic: dynamicClient, Strategy: strategy}

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
	pod, inventory := authorizePlacement(t, claim, &spec)
	placement := testPlacement(t, "new", "claim-"+string(uid), uid, spec, nil)
	ownership := testOwnership(t, path.IP, "old", "endpoint", "eni-old")
	dynamicClient := testDynamicClient(placement, ownership, inventory)
	strategy := &recordingStrategy{err: errors.New("injected placement failure")}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim, pod), Dynamic: dynamicClient, Strategy: strategy}

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

func TestPartialRouteConvergenceDoesNotTransferDurableOwnership(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("b1b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1")
	claim := testClaim("new", "endpoint", uid)
	path := model.PlacementPath{
		Name: "a", IP: "198.51.100.60/32", ENIID: "eni-new", Interface: "ens6",
		SubnetID: "subnet-a", SubnetCIDR: "10.0.1.0/24", RouteTableIDs: []string{"rtb-a", "rtb-b"},
	}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-new", Strategy: model.StrategyRouteRepoint, ForceSteal: true, Paths: []model.PlacementPath{path}}
	pod, inventory := authorizePlacement(t, claim, &spec)
	placement := testPlacement(t, claim.Namespace, "claim-"+string(uid), uid, spec, nil)
	ownership := testOwnership(t, path.IP, "old", "endpoint", "eni-old")
	dynamicClient := testDynamicClient(placement, ownership, inventory)
	strategy := &recordingStrategy{placementResults: []anchoraws.PlacementResult{{
		Err: errors.New("rtb-b did not converge"),
		RouteTables: []model.RouteTableStatus{
			{PathName: "a", RouteTableID: "rtb-a", Phase: model.RouteTableConverged, ObservedTargetType: "network-interface", ObservedTargetID: "eni-new"},
			{PathName: "a", RouteTableID: "rtb-b", Phase: model.RouteTableError, Message: "injected failure"},
		},
	}}}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim, pod), Dynamic: dynamicClient, Strategies: map[string]anchoraws.BatchPlacementStrategy{model.StrategyRouteRepoint: strategy}}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	updatedOwnership, err := reconciler.getOwnership(ctx, path.IP)
	if err != nil {
		t.Fatal(err)
	}
	if !sameLogicalOwner(updatedOwnership.Spec, "old", "endpoint") || updatedOwnership.Status.ENIID != "eni-old" {
		t.Fatalf("partial route convergence transferred ownership: %#v", updatedOwnership)
	}
	stored, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace(claim.Namespace).Get(ctx, placement.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, status, err := decodePlacement(stored)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != model.PlacementFailed || len(status.RouteTables) != 2 || status.RouteTables[0].Phase != model.RouteTableConverged || status.RouteTables[1].Phase != model.RouteTableError {
		t.Fatalf("partial route-table progress was not retained: %#v", status)
	}
}

func TestDeviceClassChangeMarksReadyPlacementStaleWithoutReapplying(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("b2b2b2b2-b2b2-b2b2-b2b2-b2b2b2b2b2b2")
	claim := testClaim("test", "endpoint", uid)
	path := model.PlacementPath{Name: "a", IP: "10.0.1.61/24", ENIID: "eni-a", Interface: "ens6", SubnetID: "subnet-a", SubnetCIDR: "10.0.1.0/24"}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-a", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{path}}
	pod, inventory := authorizePlacement(t, claim, &spec)
	claim.Spec.Devices.Requests = []resourceapi.DeviceRequest{{Name: spec.RequestName, Exactly: &resourceapi.ExactDeviceRequest{DeviceClassName: "carrier"}}}
	allocatedClass, _, err := anchorkube.AllocationParameters(claim)
	if err != nil {
		t.Fatal(err)
	}
	currentClass := allocatedClass
	currentClass.SlotsPerNode++
	currentRaw, err := json.Marshal(currentClass)
	if err != nil {
		t.Fatal(err)
	}
	deviceClass := &resourceapi.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{Name: "carrier"},
		Spec: resourceapi.DeviceClassSpec{Config: []resourceapi.DeviceClassConfiguration{{DeviceConfiguration: resourceapi.DeviceConfiguration{Opaque: &resourceapi.OpaqueDeviceConfiguration{
			Driver: constants.DriverName, Parameters: runtime.RawExtension{Raw: currentRaw},
		}}}}},
	}
	status := &model.EndpointPlacementStatus{Phase: model.PlacementReady, ObservedGeneration: 1, Paths: []model.PlacementPath{path}}
	placement := testPlacement(t, claim.Namespace, "claim-"+string(uid), uid, spec, status)
	dynamicClient := testDynamicClient(placement, inventory)
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim, pod, deviceClass), Dynamic: dynamicClient, Strategy: strategy}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(strategy.endpoints) != 0 {
		t.Fatalf("stale Ready placement was unexpectedly reapplied: %#v", strategy.endpoints)
	}
	stored, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace(claim.Namespace).Get(ctx, placement.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, storedStatus, err := decodePlacement(stored)
	if err != nil {
		t.Fatal(err)
	}
	if storedStatus.Phase != model.PlacementReady || len(storedStatus.Conditions) != 1 || storedStatus.Conditions[0].Type != "ConfigurationCurrent" || storedStatus.Conditions[0].Status != metav1.ConditionFalse {
		t.Fatalf("DeviceClass drift condition is wrong: %#v", storedStatus)
	}
}

func TestDeviceClassComparisonTreatsTopologySetsAsUnordered(t *testing.T) {
	left := model.DeviceClassParameters{Strategy: model.StrategyRouteRepoint, Profile: "carrier", SlotsPerNode: 1, Paths: []model.PathSpec{
		{Name: "b", InterfaceName: "sigtran-b", RoutingTable: 102, ENITagSelector: model.TagSelector{"path": "b"}, Subnets: []model.AWSSubnetSpec{{SubnetID: "subnet-b"}, {SubnetID: "subnet-a"}}, RouteTableIDs: []string{"rtb-b", "rtb-a"}, Routes: []string{"10.2.0.0/16", "10.1.0.0/16"}},
		{Name: "a", InterfaceName: "sigtran-a", RoutingTable: 101, ENITagSelector: model.TagSelector{"path": "a"}, Subnets: []model.AWSSubnetSpec{{SubnetID: "subnet-a"}}, RouteTableIDs: []string{"rtb-a"}},
	}}
	right := left
	right.Paths = []model.PathSpec{left.Paths[1], left.Paths[0]}
	right.Paths[1].Subnets = []model.AWSSubnetSpec{{SubnetID: "subnet-a"}, {SubnetID: "subnet-b"}}
	right.Paths[1].RouteTableIDs = []string{"rtb-a", "rtb-b"}
	right.Paths[1].Routes = []string{"10.1.0.0/16", "10.2.0.0/16"}
	if !deviceClassEquivalent(left, right) {
		t.Fatal("semantically identical DeviceClasses were reported as stale")
	}
	right.Paths[0].RoutingTable++
	if deviceClassEquivalent(left, right) {
		t.Fatal("material DeviceClass change was ignored")
	}
}

func TestForgedPlacementSpecCannotSelectAWSResources(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("bcbcbcbc-bcbc-bcbc-bcbc-bcbcbcbcbcbc")
	claim := testClaim("test", "endpoint", uid)
	authorized := model.EndpointPlacementSpec{
		ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-a", Strategy: model.StrategyIPReassign,
		Paths: []model.PlacementPath{{Name: "a", IP: "10.0.1.70/24", ENIID: "eni-authorized", Interface: "ens6", SubnetID: "subnet-a", SubnetCIDR: "10.0.1.0/24"}},
	}
	pod, inventory := authorizePlacement(t, claim, &authorized)
	forged := authorized
	forged.NodeName = "node-attacker"
	forged.Paths = append([]model.PlacementPath(nil), authorized.Paths...)
	forged.Paths[0].IP = "10.0.1.99/24"
	forged.Paths[0].ENIID = "eni-attacker"
	placement := testPlacement(t, claim.Namespace, "claim-"+string(uid), uid, forged, nil)
	strategy := &recordingStrategy{}
	dynamicClient := testDynamicClient(placement, inventory)
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim, pod), Dynamic: dynamicClient, Strategy: strategy}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(strategy.endpoints) != 0 {
		t.Fatalf("forged placement reached AWS strategy: %#v", strategy.endpoints)
	}
	stored, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace(claim.Namespace).Get(ctx, placement.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	phase, _, _ := unstructured.NestedString(stored.Object, "status", "phase")
	if phase != model.PlacementFailed {
		t.Fatalf("forged placement phase = %q, want %q", phase, model.PlacementFailed)
	}
}

func TestForceStealIsDerivedFromClaimAnnotation(t *testing.T) {
	for _, tt := range []struct {
		name           string
		claimForce     bool
		placementForce bool
	}{
		{name: "placement cannot enable force", placementForce: true},
		{name: "claim can enable force", claimForce: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			uid := types.UID("cdcdcdcd-cdcd-cdcd-cdcd-" + strings.ReplaceAll(tt.name, " ", "-"))
			claim := testClaim("test", "endpoint-"+strings.ReplaceAll(tt.name, " ", "-"), uid)
			spec := model.EndpointPlacementSpec{
				ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-a", Strategy: model.StrategyIPReassign,
				Paths: []model.PlacementPath{{Name: "a", IP: "10.0.1.71/24", ENIID: "eni-a", Interface: "ens6", SubnetID: "subnet-a", SubnetCIDR: "10.0.1.0/24"}},
			}
			pod, inventory := authorizePlacement(t, claim, &spec)
			if tt.claimForce {
				claim.Annotations = map[string]string{constants.ForceStealAnnotation: "true"}
			}
			spec.ForceSteal = tt.placementForce
			placement := testPlacement(t, claim.Namespace, "claim-"+string(uid), uid, spec, nil)
			strategy := &recordingStrategy{}
			reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim, pod), Dynamic: testDynamicClient(placement, inventory), Strategy: strategy}

			if err := reconciler.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if len(strategy.endpoints) != 1 || strategy.endpoints[0].ForceSteal != tt.claimForce {
				t.Fatalf("strategy force-steal = %#v, want %t", strategy.endpoints, tt.claimForce)
			}
		})
	}
}

func TestPlacementRequiresAllocationOnReservedPodNode(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("dededede-dede-dede-dede-dededededede")
	claim := testClaim("test", "endpoint", uid)
	spec := model.EndpointPlacementSpec{
		ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-a", Strategy: model.StrategyIPReassign,
		Paths: []model.PlacementPath{{Name: "a", IP: "10.0.1.72/24", ENIID: "eni-a", Interface: "ens6", SubnetID: "subnet-a", SubnetCIDR: "10.0.1.0/24"}},
	}
	pod, inventory := authorizePlacement(t, claim, &spec)
	pod.Spec.NodeName = "node-b"
	placement := testPlacement(t, claim.Namespace, "claim-"+string(uid), uid, spec, nil)
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim, pod), Dynamic: testDynamicClient(placement, inventory), Strategy: strategy}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(strategy.endpoints) != 0 {
		t.Fatalf("mismatched pod node reached AWS strategy: %#v", strategy.endpoints)
	}
}

func TestPlacementGetsOnlyReservedPod(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("efefefef-efef-efef-efef-efefefefefef")
	claim := testClaim("test", "endpoint", uid)
	spec := model.EndpointPlacementSpec{
		ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-a", Strategy: model.StrategyIPReassign,
		Paths: []model.PlacementPath{{Name: "a", IP: "10.0.1.73/24", ENIID: "eni-a", Interface: "ens6", SubnetID: "subnet-a", SubnetCIDR: "10.0.1.0/24"}},
	}
	pod, inventory := authorizePlacement(t, claim, &spec)
	unrelated := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "other", UID: "unrelated-uid"}}
	placement := testPlacement(t, claim.Namespace, "claim-"+string(uid), uid, spec, nil)
	coreClient := fake.NewSimpleClientset(claim, pod, unrelated)
	reconciler := &PlacementReconciler{Core: coreClient, Dynamic: testDynamicClient(placement, inventory), Strategy: &recordingStrategy{}}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countActions(coreClient.Actions(), "list", "pods", ""); got != 0 {
		t.Fatalf("reconciliation listed Pods %d times, want 0", got)
	}
	if got := countActions(coreClient.Actions(), "get", "pods", ""); got != 1 {
		t.Fatalf("reconciliation got Pods %d times, want 1", got)
	}
}

func TestReservedPodLookupIsCachedPerReconciliation(t *testing.T) {
	ctx := context.Background()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "test", UID: "pod-uid"}}
	claims := []*resourceapi.ResourceClaim{
		{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "test"}, Status: resourceapi.ResourceClaimStatus{ReservedFor: []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: pod.Name, UID: pod.UID}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "test"}, Status: resourceapi.ResourceClaimStatus{ReservedFor: []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: pod.Name, UID: pod.UID}}}},
	}
	coreClient := fake.NewSimpleClientset(pod)
	reconciler := &PlacementReconciler{Core: coreClient}
	cache := map[string]podLookupResult{}

	for _, claim := range claims {
		_, handoff, err := reconciler.getReservedPod(ctx, claim, cache)
		if err != nil {
			t.Fatal(err)
		}
		if handoff {
			t.Fatal("matching reserved Pod was classified as a handoff")
		}
	}
	if got := countActions(coreClient.Actions(), "get", "pods", ""); got != 1 {
		t.Fatalf("shared reserving Pod was fetched %d times, want 1", got)
	}
}

func TestPodHandoffRetainsReadyPlacement(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*corev1.Pod)
		omit   bool
	}{
		{name: "missing", omit: true},
		{name: "recreated UID", mutate: func(pod *corev1.Pod) { pod.UID = "replacement-uid" }},
		{name: "terminating", mutate: func(pod *corev1.Pod) { now := metav1.Now(); pod.DeletionTimestamp = &now }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			uid := types.UID("f0f0f0f0-f0f0-f0f0-f0f0-" + strings.ReplaceAll(strings.ToLower(tt.name), " ", "-"))
			claim := testClaim("test", "endpoint", uid)
			spec := model.EndpointPlacementSpec{
				ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-a", Strategy: model.StrategyIPReassign,
				Paths: []model.PlacementPath{{Name: "a", IP: "10.0.1.74/24", ENIID: "eni-a", Interface: "ens6", SubnetID: "subnet-a", SubnetCIDR: "10.0.1.0/24"}},
			}
			pod, inventory := authorizePlacement(t, claim, &spec)
			if tt.mutate != nil {
				tt.mutate(pod)
			}
			status := &model.EndpointPlacementStatus{Phase: model.PlacementReady, ObservedGeneration: 1, Paths: spec.Paths}
			placement := testPlacement(t, claim.Namespace, "claim-"+string(uid), uid, spec, status)
			coreObjects := []runtime.Object{claim}
			if !tt.omit {
				coreObjects = append(coreObjects, pod)
			}
			strategy := &recordingStrategy{}
			coreClient := fake.NewSimpleClientset(coreObjects...)
			dynamicClient := testDynamicClient(placement, inventory)
			reconciler := &PlacementReconciler{Core: coreClient, Dynamic: dynamicClient, Strategy: strategy}

			if err := reconciler.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if len(strategy.endpoints) != 0 {
				t.Fatalf("Pod handoff reached AWS strategy: %#v", strategy.endpoints)
			}
			if got := countActions(dynamicClient.Actions(), "update", "endpointplacements", "status"); got != 0 {
				t.Fatalf("handoff updated placement status %d times, want 0", got)
			}
			if got := countActions(coreClient.Actions(), "create", "events", ""); got != 0 {
				t.Fatalf("handoff emitted %d events, want 0", got)
			}
			stored, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace(claim.Namespace).Get(ctx, placement.GetName(), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			phase, _, _ := unstructured.NestedString(stored.Object, "status", "phase")
			if phase != model.PlacementReady {
				t.Fatalf("handoff changed placement phase to %q", phase)
			}
		})
	}
}

func TestMissingReservedPodDoesNotBlockOtherPlacement(t *testing.T) {
	ctx := context.Background()
	validClaim := testClaim("valid", "endpoint", types.UID("01010101-0101-0101-0101-010101010101"))
	missingClaim := testClaim("missing", "endpoint", types.UID("02020202-0202-0202-0202-020202020202"))
	validSpec := model.EndpointPlacementSpec{
		ClaimName: validClaim.Name, ClaimUID: string(validClaim.UID), NodeName: "node-valid", Strategy: model.StrategyIPReassign,
		Paths: []model.PlacementPath{{Name: "a", IP: "10.0.1.75/24", ENIID: "eni-valid", Interface: "ens6", SubnetID: "subnet-a", SubnetCIDR: "10.0.1.0/24"}},
	}
	missingSpec := model.EndpointPlacementSpec{
		ClaimName: missingClaim.Name, ClaimUID: string(missingClaim.UID), NodeName: "node-missing", Strategy: model.StrategyIPReassign,
		Paths: []model.PlacementPath{{Name: "a", IP: "10.0.1.76/24", ENIID: "eni-missing", Interface: "ens6", SubnetID: "subnet-a", SubnetCIDR: "10.0.1.0/24"}},
	}
	validPod, validInventory := authorizePlacement(t, validClaim, &validSpec)
	_, missingInventory := authorizePlacement(t, missingClaim, &missingSpec)
	validPlacement := testPlacement(t, validClaim.Namespace, "claim-"+string(validClaim.UID), validClaim.UID, validSpec, nil)
	missingPlacement := testPlacement(t, missingClaim.Namespace, "claim-"+string(missingClaim.UID), missingClaim.UID, missingSpec, nil)
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{
		Core: fake.NewSimpleClientset(validClaim, missingClaim, validPod),
		Dynamic: testDynamicClient(
			validPlacement, missingPlacement, validInventory, missingInventory,
		),
		Strategy: strategy,
	}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(strategy.endpoints) != 1 || strategy.endpoints[0].ClaimName != "valid/endpoint" {
		t.Fatalf("valid placement was blocked by missing Pod: %#v", strategy.endpoints)
	}
}

func TestNodeInventoryStatusCannotChangeAWSIdentity(t *testing.T) {
	inventory := &placementInventory{
		Spec:   model.AnchorNodeInventorySpec{Profiles: map[string][]model.ENIPath{"carrier": {{Name: "a", ENIID: "eni-authorized", SubnetID: "subnet-a"}}}},
		Status: model.AnchorNodeInventoryStatus{Interfaces: map[string][]model.ENIPath{"carrier": {{Name: "a", ENIID: "eni-forged", SubnetID: "subnet-a", Interface: "ens6"}}}},
	}
	if _, _, err := trustedInventoryPaths(inventory, "carrier"); err == nil || !strings.Contains(err.Error(), "does not match controller inventory") {
		t.Fatalf("expected forged inventory status to be rejected, got %v", err)
	}
}

func TestAmbiguousLegacyOwnershipFailsWithoutGuessing(t *testing.T) {
	ctx := context.Background()
	address := "10.0.1.80/24"
	objects := []runtime.Object{}
	coreObjects := []runtime.Object{}
	for i, namespace := range []string{"first", "second"} {
		uid := types.UID(fmt.Sprintf("dddddddd-dddd-dddd-dddd-dddddddddd%02d", i))
		claim := testClaim(namespace, "endpoint", uid)
		path := model.PlacementPath{Name: "a", IP: address, ENIID: "eni-" + namespace, Interface: "ens6", SubnetID: "subnet-a"}
		spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-" + namespace, Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{path}}
		pod, inventory := authorizePlacement(t, claim, &spec)
		status := &model.EndpointPlacementStatus{Phase: model.PlacementReady, ObservedGeneration: 1, Paths: append([]model.PlacementPath(nil), spec.Paths...)}
		objects = append(objects, testPlacement(t, namespace, "claim-"+string(uid), uid, spec, status))
		objects = append(objects, inventory)
		coreObjects = append(coreObjects, claim, pod)
	}
	dynamicClient := testDynamicClient(objects...)
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(coreObjects...), Dynamic: dynamicClient, Strategy: &recordingStrategy{}}

	err := reconciler.Reconcile(ctx)
	if err == nil || !strings.Contains(err.Error(), "ambiguous legacy owners") {
		t.Fatalf("expected ambiguous migration failure, got %v", err)
	}
	if _, err := reconciler.getOwnership(ctx, address); !apierrors.IsNotFound(err) {
		t.Fatalf("ambiguous migration created ownership: %v", err)
	}
}

func TestFailureStatusAndEventsAreNotRepeated(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	claim := testClaim("test", "endpoint", uid)
	path := model.PlacementPath{Name: "a", IP: "10.0.1.90/24", ENIID: "eni-new", Interface: "ens6", SubnetID: "subnet-a"}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-new", Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{path}}
	pod, inventory := authorizePlacement(t, claim, &spec)
	placement := testPlacement(t, "test", "claim-"+string(uid), uid, spec, nil)
	ownership := testOwnership(t, path.IP, "test", claim.Name, "eni-old")
	dynamicClient := testDynamicClient(placement, ownership, inventory)
	coreClient := fake.NewSimpleClientset(claim, pod)
	strategy := &recordingStrategy{err: errors.New("first failure")}
	reconciler := &PlacementReconciler{Core: coreClient, Dynamic: dynamicClient, Strategy: strategy}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countActions(dynamicClient.Actions(), "update", "endpointplacements", "status"); got != 1 {
		t.Fatalf("identical failure wrote status %d times, want 1", got)
	}
	if got := countActions(coreClient.Actions(), "create", "events", ""); got != 1 {
		t.Fatalf("identical failure emitted %d events, want 1", got)
	}

	strategy.err = errors.New("changed failure")
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countActions(dynamicClient.Actions(), "update", "endpointplacements", "status"); got != 2 {
		t.Fatalf("changed failure wrote status %d times, want 2", got)
	}
	if got := countActions(coreClient.Actions(), "create", "events", ""); got != 1 {
		t.Fatalf("changed failure emitted %d events, want 1", got)
	}

	strategy.err = nil
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countActions(dynamicClient.Actions(), "update", "endpointplacements", "status"); got != 3 {
		t.Fatalf("recovery wrote status %d times, want 3", got)
	}
	if got := countActions(coreClient.Actions(), "create", "events", ""); got != 2 {
		t.Fatalf("recovery emitted %d events, want 2", got)
	}
}

func TestPlacementPathsAreSorted(t *testing.T) {
	paths := placementPaths(map[string]model.PlacementPath{
		"b": {Name: "b"},
		"a": {Name: "a"},
	})
	if len(paths) != 2 || paths[0].Name != "a" || paths[1].Name != "b" {
		t.Fatalf("paths are not sorted: %#v", paths)
	}
}

func TestReadyPlacementIsOnlyCheckedByDriftReconcile(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("12121212-1212-1212-1212-121212121212")
	claim := testClaim("test", "endpoint", uid)
	paths := []model.PlacementPath{
		{Name: "a", IP: "10.0.1.110/24", ENIID: "eni-a", Interface: "ens6", SubnetID: "subnet-a"},
		{Name: "b", IP: "10.0.2.110/24", ENIID: "eni-b", Interface: "ens7", SubnetID: "subnet-b"},
	}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-a", Strategy: model.StrategyIPReassign, Paths: paths}
	pod, inventory := authorizePlacement(t, claim, &spec)
	status := &model.EndpointPlacementStatus{Phase: model.PlacementReady, ObservedGeneration: 1, Paths: paths}
	placement := testPlacement(t, claim.Namespace, "claim-"+string(uid), uid, spec, status)
	dynamicClient := testDynamicClient(placement, inventory, testOwnership(t, paths[0].IP, claim.Namespace, claim.Name, paths[0].ENIID), testOwnership(t, paths[1].IP, claim.Namespace, claim.Name, paths[1].ENIID))
	strategy := &recordingStrategy{}
	reconciler := &PlacementReconciler{Core: fake.NewSimpleClientset(claim, pod), Dynamic: dynamicClient, Strategy: strategy}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(strategy.endpoints) != 0 || strategy.verifyCalls != 0 {
		t.Fatalf("normal reconcile touched Ready placement: placed=%d verified=%d", len(strategy.endpoints), strategy.verifyCalls)
	}
	if err := reconciler.VerifyReady(ctx); err != nil {
		t.Fatal(err)
	}
	if strategy.verifyCalls != 1 || len(strategy.verified) != 2 || len(strategy.endpoints) != 0 {
		t.Fatalf("unexpected drift check: calls=%d paths=%d repairs=%d", strategy.verifyCalls, len(strategy.verified), len(strategy.endpoints))
	}

	strategy.verifyErr = errors.New("injected batch failure")
	if err := reconciler.VerifyReady(ctx); err == nil {
		t.Fatal("expected batch verification failure")
	}
	stored, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace(claim.Namespace).Get(ctx, placement.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	phase, _, _ := unstructured.NestedString(stored.Object, "status", "phase")
	if phase != model.PlacementReady || len(strategy.endpoints) != 0 {
		t.Fatalf("batch failure changed Ready placement: phase=%s repairs=%d", phase, len(strategy.endpoints))
	}
	strategy.verifyErr = nil
	strategy.verification = []anchoraws.VerificationResult{{Placed: false}, {Placed: true}}
	if err := reconciler.VerifyReady(ctx); err != nil {
		t.Fatal(err)
	}
	if len(strategy.endpoints) != 1 {
		t.Fatalf("drifted endpoint repaired %d paths, want only the drifted path", len(strategy.endpoints))
	}
}
