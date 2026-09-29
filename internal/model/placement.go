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
	Name                  string   `json:"name"`
	InterfaceName         string   `json:"interfaceName"`
	RoutingTable          int      `json:"routingTable"`
	IP                    string   `json:"ip"`
	ENIID                 string   `json:"eniId"`
	ParentMAC             string   `json:"parentMAC"`
	Interface             string   `json:"interface"`
	SubnetID              string   `json:"subnetId"`
	SubnetCIDR            string   `json:"subnetCidr"`
	Gateway               string   `json:"gateway,omitempty"`
	Routes                []string `json:"routes,omitempty"`
	PreferredDestinations []string `json:"preferredDestinations,omitempty"`
	RouteTableIDs         []string `json:"routeTableIds,omitempty"`
}

// BuildPlacementPath combines tenant addressing with an inventoried ENI and
// admin-owned routing configuration.
func BuildPlacementPath(address AddressSpec, path ENIPath, configured PathSpec) PlacementPath {
	resolved, _ := configured.ResolveAWSSubnet(path.SubnetID)
	gateway := configured.Gateway
	if resolved.Gateway != "" {
		gateway = resolved.Gateway
	}
	return PlacementPath{
		Name: path.Name, InterfaceName: configured.InterfaceName, RoutingTable: configured.RoutingTable,
		IP: address.IP, ENIID: path.ENIID, ParentMAC: path.MAC, Interface: path.Interface,
		SubnetID: path.SubnetID, SubnetCIDR: path.SubnetCIDR,
		Gateway: gateway, Routes: append([]string(nil), configured.Routes...),
		PreferredDestinations: append([]string(nil), configured.PreferredDestinations...),
		RouteTableIDs:         append([]string(nil), configured.RouteTableIDs...),
	}
}

type EndpointPlacementSpec struct {
	ClaimName     string          `json:"claimName"`
	ClaimUID      string          `json:"claimUID"`
	OwnershipName string          `json:"ownershipName,omitempty"`
	NodeName      string          `json:"nodeName"`
	RequestName   string          `json:"requestName"`
	PoolName      string          `json:"poolName"`
	DeviceName    string          `json:"deviceName"`
	Strategy      string          `json:"strategy"`
	ForceSteal    bool            `json:"forceSteal,omitempty"`
	Paths         []PlacementPath `json:"paths"`
}

type EndpointPlacementStatus struct {
	Phase              string             `json:"phase,omitempty"`
	Message            string             `json:"message,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	PlacedAt           *metav1.Time       `json:"placedAt,omitempty"`
	Paths              []PlacementPath    `json:"paths,omitempty"`
	Injection          *InjectionStatus   `json:"injection,omitempty"`
	RouteTables        []RouteTableStatus `json:"routeTables,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

type RouteTableStatus struct {
	PathName           string       `json:"pathName"`
	RouteTableID       string       `json:"routeTableId"`
	Phase              string       `json:"phase"`
	ObservedTargetType string       `json:"observedTargetType,omitempty"`
	ObservedTargetID   string       `json:"observedTargetId,omitempty"`
	LastAttemptAt      *metav1.Time `json:"lastAttemptAt,omitempty"`
	Message            string       `json:"message,omitempty"`
}

const (
	RouteTablePending   = "Pending"
	RouteTableConverged = "Converged"
	RouteTableConflict  = "Conflict"
	RouteTableError     = "Error"
)

type InjectionStatus struct {
	Phase          string       `json:"phase,omitempty"`
	FirstFailureAt *metav1.Time `json:"firstFailureAt,omitempty"`
	LastAttemptAt  *metav1.Time `json:"lastAttemptAt,omitempty"`
	LastError      string       `json:"lastError,omitempty"`
	RetryCount     int64        `json:"retryCount,omitempty"`
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
