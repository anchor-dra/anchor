package model

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type ENIPath struct {
	Name        string            `json:"name"`
	ENIID       string            `json:"eniId"`
	MAC         string            `json:"mac"`
	SubnetID    string            `json:"subnetId"`
	SubnetCIDR  string            `json:"subnetCidr"`
	Tags        map[string]string `json:"tags,omitempty"`
	Interface   string            `json:"interface,omitempty"`
	PrimaryIPv4 string            `json:"primaryIPv4,omitempty"`
}

type AnchorNodeInventorySpec struct {
	NodeName     string               `json:"nodeName"`
	InstanceID   string               `json:"instanceId"`
	AZ           string               `json:"availabilityZone"`
	Profiles     map[string][]ENIPath `json:"profiles"`
	SlotsPerNode map[string]int       `json:"slotsPerNode"`
}

type AnchorNodeInventoryStatus struct {
	Ready              bool                 `json:"ready"`
	ObservedGeneration int64                `json:"observedGeneration,omitempty"`
	Interfaces         map[string][]ENIPath `json:"interfaces,omitempty"`
	Message            string               `json:"message,omitempty"`
}

type PlacementPath struct {
	Name       string `json:"name"`
	IP         string `json:"ip"`
	ENIID      string `json:"eniId"`
	Interface  string `json:"interface"`
	SubnetID   string `json:"subnetId"`
	SubnetCIDR string `json:"subnetCidr"`
}

type EndpointPlacementSpec struct {
	ClaimName   string          `json:"claimName"`
	ClaimUID    string          `json:"claimUID"`
	NodeName    string          `json:"nodeName"`
	RequestName string          `json:"requestName"`
	PoolName    string          `json:"poolName"`
	DeviceName  string          `json:"deviceName"`
	Strategy    string          `json:"strategy"`
	ForceSteal  bool            `json:"forceSteal,omitempty"`
	Paths       []PlacementPath `json:"paths"`
}

type EndpointPlacementStatus struct {
	Phase              string          `json:"phase,omitempty"`
	Message            string          `json:"message,omitempty"`
	ObservedGeneration int64           `json:"observedGeneration,omitempty"`
	PlacedAt           *metav1.Time    `json:"placedAt,omitempty"`
	Paths              []PlacementPath `json:"paths,omitempty"`
}

// EndpointOwnership is the durable identity of one static address. Unlike an
// EndpointPlacement, it is cluster-scoped and intentionally outlives both a
// ResourceClaim incarnation and its namespace.
type EndpointOwnershipSpec struct {
	Address        string `json:"address"`
	ClaimNamespace string `json:"claimNamespace"`
	ClaimName      string `json:"claimName"`
}

type EndpointOwnershipStatus struct {
	ClaimUID     string       `json:"claimUID,omitempty"`
	PathName     string       `json:"pathName,omitempty"`
	ENIID        string       `json:"eniId,omitempty"`
	NodeName     string       `json:"nodeName,omitempty"`
	LastPlacedAt *metav1.Time `json:"lastPlacedAt,omitempty"`
}

const (
	PlacementPending = "Pending"
	PlacementReady   = "Ready"
	PlacementFailed  = "Failed"
)
