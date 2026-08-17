package model

import "testing"

func validClass() DeviceClassParameters {
	return DeviceClassParameters{
		Profile: "carrier-dual",
		Paths: []PathSpec{
			{Name: "a", SubnetID: "subnet-a", ENITagSelector: TagSelector{"path": "a"}},
			{Name: "b", SubnetID: "subnet-b", ENITagSelector: TagSelector{"path": "b"}},
		},
	}
}

func TestNormalizeAndValidate(t *testing.T) {
	class := validClass()
	if err := class.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	if class.Strategy != StrategyIPReassign || class.SlotsPerNode != 1 {
		t.Fatalf("defaults not applied: %#v", class)
	}
}

func TestClaimValidation(t *testing.T) {
	class := validClass()
	_ = class.NormalizeAndValidate()
	claim := ClaimParameters{Addresses: []AddressSpec{{Path: "a", IP: "10.0.1.10/24"}, {Path: "b", IP: "10.0.2.10/24"}}}
	err := claim.Validate(class, map[string]string{"subnet-a": "10.0.1.0/24", "subnet-b": "10.0.2.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	claim.Addresses[1].IP = "10.0.3.10/24"
	if err := claim.Validate(class, map[string]string{"subnet-a": "10.0.1.0/24", "subnet-b": "10.0.2.0/24"}); err == nil {
		t.Fatal("expected out-of-subnet validation error")
	}
	claim.Addresses[1].IP = "10.0.2.2/24"
	if err := claim.Validate(class, map[string]string{"subnet-a": "10.0.1.0/24", "subnet-b": "10.0.2.0/24"}); err == nil {
		t.Fatal("expected AWS-reserved address validation error")
	}
}

func TestTagsMatch(t *testing.T) {
	if !TagsMatch(map[string]string{"carrier": "true", "path": "a"}, TagSelector{"path": "a"}) {
		t.Fatal("expected tag selector to match")
	}
	if TagsMatch(map[string]string{"path": "b"}, TagSelector{"path": "a"}) {
		t.Fatal("unexpected tag selector match")
	}
}
