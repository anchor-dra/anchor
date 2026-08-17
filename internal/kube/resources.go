package kube

import "k8s.io/apimachinery/pkg/runtime/schema"

var (
	InventoryGVR = schema.GroupVersionResource{Group: "anchor.dra.example.com", Version: "v1alpha1", Resource: "anchornodeinventories"}
	PlacementGVR = schema.GroupVersionResource{Group: "anchor.dra.example.com", Version: "v1alpha1", Resource: "endpointplacements"}
	NADGVR       = schema.GroupVersionResource{Group: "k8s.cni.cncf.io", Version: "v1", Resource: "network-attachment-definitions"}
)
