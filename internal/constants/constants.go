package constants

const (
	DriverName             = "dra.anchordra.co"
	APIGroup               = "dra.anchordra.co"
	APIVersion             = "v1alpha1"
	SystemNamespace        = "anchor-system"
	ForceStealAnnotation   = DriverName + "/force-steal"
	FencedAnnotation       = DriverName + "/fenced"
	NRIPluginName          = "anchor"
	NRIBootstrapAnnotation = DriverName + "/tolerate-missing-nri-plugin"
	NotReadyTaintKey       = DriverName + "/not-ready"
)
