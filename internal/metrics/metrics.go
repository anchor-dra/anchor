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
	RouteTableUpdates = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "anchor", Name: "route_table_updates_total", Help: "Route-repoint table operations by operation and result.",
	}, []string{"operation", "result"})
	ReconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "anchor", Name: "reconcile_errors_total", Help: "Reconciliation errors by controller.",
	}, []string{"controller"})
	ReconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "anchor", Name: "reconcile_duration_seconds", Help: "Reconciliation duration by controller.", Buckets: prometheus.DefBuckets,
	}, []string{"controller"})
	DriftDetections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "anchor", Name: "drift_detections_total", Help: "Ready placements found to have drifted.",
	}, []string{"strategy"})
	Injections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "anchor", Name: "network_injections_total", Help: "NRI network injection attempts by result.",
	}, []string{"result"})
	InjectionRetries = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "anchor", Name: "network_injection_retries_total", Help: "Network injection retries after an initial failure.",
	})
	InjectionFailureAge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "anchor", Name: "network_injection_failure_age_seconds", Help: "Age of the most recently observed persistent network injection failure.",
	})
)

func init() {
	prometheus.MustRegister(Placements, PlacementDuration, EC2Throttles, RouteTableUpdates, ReconcileErrors, ReconcileDuration, DriftDetections, Injections, InjectionRetries, InjectionFailureAge)
}
