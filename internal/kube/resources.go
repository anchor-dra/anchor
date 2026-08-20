package kube

import "k8s.io/apimachinery/pkg/runtime/schema"

var (
	InventoryGVR = schema.GroupVersionResource{Group: "dra.anchordra.co", Version: "v1alpha1", Resource: "anchornodeinventories"}
	PlacementGVR = schema.GroupVersionResource{Group: "dra.anchordra.co", Version: "v1alpha1", Resource: "endpointplacements"}
	OwnershipGVR = schema.GroupVersionResource{Group: "dra.anchordra.co", Version: "v1alpha1", Resource: "endpointownerships"}
)
