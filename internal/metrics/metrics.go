package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	Placements = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "anchor", Name: "placements_total", Help: "AWS placement attempts by strategy and result.",
	}, []string{"strategy", "result"})
	PlacementDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "anchor", Name: "placement_duration_seconds", Help: "AWS placement latency.", Buckets: prometheus.DefBuckets,
	}, []string{"strategy"})
	EC2Throttles = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "anchor", Name: "ec2_throttle_events_total", Help: "EC2 throttling responses observed by Anchor.",
	}, []string{"operation"})
	ReconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "anchor", Name: "reconcile_errors_total", Help: "Reconciliation errors by controller.",
	}, []string{"controller"})
	DriftDetections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "anchor", Name: "drift_detections_total", Help: "Ready placements found to have drifted.",
	}, []string{"strategy"})
)

func init() {
	prometheus.MustRegister(Placements, PlacementDuration, EC2Throttles, ReconcileErrors, DriftDetections)
}
