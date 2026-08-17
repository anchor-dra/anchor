package node

import (
	"testing"

	"example.com/anchor/internal/model"
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
		profile := device.Attributes["anchor.dra.example.com/profile"]
		if profile.StringValue == nil || *profile.StringValue != "dual" {
			t.Fatalf("missing profile attribute: %#v", device)
		}
		availabilityZone := device.Attributes["anchor.dra.example.com/availability_zone"]
		if availabilityZone.StringValue == nil || *availabilityZone.StringValue != "eu-west-1a" {
			t.Fatalf("missing availability-zone attribute: %#v", device)
		}
	}
}
