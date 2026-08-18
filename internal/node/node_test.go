package node

import (
	"context"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/anchor-dra/anchor/internal/constants"
	anchorkube "github.com/anchor-dra/anchor/internal/kube"
	"github.com/anchor-dra/anchor/internal/model"
)

func TestResourcesRequireReadyMappedPaths(t *testing.T) {
	spec := model.AnchorNodeInventorySpec{NodeName: "node-a", AZ: "eu-west-1a", SlotsPerNode: map[string]int{"dual": 2}}
	status := model.AnchorNodeInventoryStatus{Ready: true, Interfaces: map[string][]model.ENIPath{"dual": {{Name: "a", Interface: "ens6"}, {Name: "b", Interface: "ens7"}}}}
	resources := resourcesForInventory(spec, status)
	pool := resources.Pools["node-a"]
	if len(pool.Slices) != 1 || len(pool.Slices[0].Devices) != 2 {
		t.Fatalf("unexpected resources: %#v", resources)
	}
	for _, device := range pool.Slices[0].Devices {
		profile := device.Attributes["dra.anchordra.co/profile"]
		if profile.StringValue == nil || *profile.StringValue != "dual" {
			t.Fatalf("missing profile attribute: %#v", device)
		}
		availabilityZone := device.Attributes["dra.anchordra.co/availability_zone"]
		if availabilityZone.StringValue == nil || *availabilityZone.StringValue != "eu-west-1a" {
			t.Fatalf("missing availability-zone attribute: %#v", device)
		}
	}
}

func TestConfiguredPlacementPathIncludesRouting(t *testing.T) {
	address := model.AddressSpec{Path: "a", IP: "10.0.1.10/24"}
	path := model.ENIPath{Name: "a", ENIID: "eni-a", Interface: "ens6", SubnetID: "subnet-a", SubnetCIDR: "10.0.1.0/24"}
	configured := model.PathSpec{Name: "a", Gateway: "10.0.1.1", Routes: []string{"192.0.2.0/24"}}

	result := configuredPlacementPath(address, path, configured)
	if result.Gateway != configured.Gateway || len(result.Routes) != 1 || result.Routes[0] != configured.Routes[0] {
		t.Fatalf("routing was not copied to placement: %#v", result)
	}
}

func TestPlacementIsOwnedByExactClaimIncarnation(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: "test", UID: uid}}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: "node-a", Strategy: model.StrategyIPReassign}
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	plugin := &Plugin{dynamic: dynamicClient}
	name := "claim-" + string(uid)

	if _, err := plugin.upsertPlacement(ctx, claim, name, spec); err != nil {
		t.Fatal(err)
	}
	placement, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace(claim.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owners := placement.GetOwnerReferences()
	if len(owners) != 1 || owners[0].APIVersion != "resource.k8s.io/v1" || owners[0].Kind != "ResourceClaim" || owners[0].Name != claim.Name || owners[0].UID != uid || owners[0].Controller == nil || !*owners[0].Controller {
		t.Fatalf("unexpected owner reference: %#v", owners)
	}
	if placement.GetLabels()[constants.DriverName+"/claim-uid"] != string(uid) {
		t.Fatalf("missing claim UID label: %#v", placement.GetLabels())
	}
	placement.SetOwnerReferences(nil)
	placement.SetLabels(nil)
	if _, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace(claim.Namespace).Update(ctx, placement, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.upsertPlacement(ctx, claim, name, spec); err != nil {
		t.Fatal(err)
	}
	repaired, err := dynamicClient.Resource(anchorkube.PlacementGVR).Namespace(claim.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired.GetOwnerReferences()) != 1 || repaired.GetOwnerReferences()[0].UID != uid || repaired.GetLabels()[constants.DriverName+"/claim-uid"] != string(uid) {
		t.Fatalf("existing placement metadata was not repaired: owners=%#v labels=%#v", repaired.GetOwnerReferences(), repaired.GetLabels())
	}
}
