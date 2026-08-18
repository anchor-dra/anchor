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

func TestPathRoutingValidation(t *testing.T) {
	valid := validClass()
	valid.Paths[0].Gateway = "10.0.1.1"
	valid.Paths[0].Routes = []string{"0.0.0.0/0", "192.0.2.0/24"}
	if err := valid.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		gateway string
		routes  []string
	}{
		{name: "gateway without routes", gateway: "10.0.1.1"},
		{name: "routes without gateway", routes: []string{"192.0.2.0/24"}},
		{name: "invalid gateway", gateway: "not-an-ip", routes: []string{"192.0.2.0/24"}},
		{name: "invalid route", gateway: "10.0.1.1", routes: []string{"not-a-cidr"}},
		{name: "non-canonical route", gateway: "10.0.1.1", routes: []string{"192.0.2.1/24"}},
		{name: "duplicate route", gateway: "10.0.1.1", routes: []string{"192.0.2.0/24", "192.0.2.0/24"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			class := validClass()
			class.Paths[0].Gateway = tt.gateway
			class.Paths[0].Routes = tt.routes
			if err := class.NormalizeAndValidate(); err == nil {
				t.Fatal("expected invalid routing configuration")
			}
		})
	}
}

func TestClaimRoutingValidation(t *testing.T) {
	claim := ClaimParameters{Addresses: []AddressSpec{{Path: "a", IP: "10.0.1.10/24"}, {Path: "b", IP: "10.0.2.10/24"}}}
	subnets := map[string]string{"subnet-a": "10.0.1.0/24", "subnet-b": "10.0.2.0/24"}

	class := validClass()
	class.Paths[0].Gateway = "10.0.3.1"
	class.Paths[0].Routes = []string{"192.0.2.0/24"}
	if err := class.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	if err := claim.Validate(class, map[string]string{"subnet-b": "10.0.2.0/24"}); err == nil {
		t.Fatal("expected missing discovered subnet error")
	}
	if err := claim.Validate(class, subnets); err == nil {
		t.Fatal("expected out-of-subnet gateway error")
	}

	class.Paths[0].Gateway = "10.0.1.10"
	if err := claim.Validate(class, subnets); err == nil {
		t.Fatal("expected gateway/address conflict")
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
