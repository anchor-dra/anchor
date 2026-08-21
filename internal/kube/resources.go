package kube

import (
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const podClaimNameAnnotation = "resource.kubernetes.io/pod-claim-name"

var (
	InventoryGVR = schema.GroupVersionResource{Group: "dra.anchordra.co", Version: "v1alpha1", Resource: "anchornodeinventories"}
	PlacementGVR = schema.GroupVersionResource{Group: "dra.anchordra.co", Version: "v1alpha1", Resource: "endpointplacements"}
	OwnershipGVR = schema.GroupVersionResource{Group: "dra.anchordra.co", Version: "v1alpha1", Resource: "endpointownerships"}
)

// ClaimOwnershipName returns the durable logical name of a claim. Claims
// generated from a ResourceClaimTemplate get a new API name on every Pod
// incarnation, so their stable identity is the reserving Pod name plus the
// Pod resource-claim alias. Standalone ResourceClaims return an empty override,
// which means their API name remains authoritative.
func ClaimOwnershipName(claim *resourceapi.ResourceClaim, pod *corev1.Pod) string {
	alias := claim.Annotations[podClaimNameAnnotation]
	if alias == "" || pod == nil {
		return ""
	}
	for _, owner := range claim.OwnerReferences {
		if owner.Controller != nil && *owner.Controller && owner.APIVersion == "v1" && owner.Kind == "Pod" && owner.Name == pod.Name && owner.UID == pod.UID {
			return pod.Name + "-" + alias
		}
	}
	return ""
}
