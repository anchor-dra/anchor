package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestClaimOwnershipName(t *testing.T) {
	controller := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "test", UID: types.UID("pod-uid")}}
	generated := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      "workload-endpoint-abcde",
		Namespace: "test",
		UID:       types.UID("claim-uid"),
		Annotations: map[string]string{
			podClaimNameAnnotation: "endpoint",
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID, Controller: &controller,
		}},
	}}
	if got := ClaimOwnershipName(generated, pod); got != "workload-endpoint" {
		t.Fatalf("generated claim ownership name = %q", got)
	}

	standalone := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: "static-endpoint", Namespace: "test"}}
	if got := ClaimOwnershipName(standalone, pod); got != "" {
		t.Fatalf("standalone claim ownership name = %q", got)
	}

	generated.OwnerReferences[0].UID = types.UID("different-pod")
	if got := ClaimOwnershipName(generated, pod); got != "" {
		t.Fatalf("unverified generated claim ownership name = %q", got)
	}
}
