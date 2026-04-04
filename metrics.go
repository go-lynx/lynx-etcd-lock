package etcdlock

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricsNamespace = "lynx"
	metricsSubsystem = "etcd_lock"
)

var (
	metricsRegisterOnce sync.Once

	lockOperationTotal *prometheus.CounterVec
	lockOperationDur   *prometheus.HistogramVec
)

var metricOperations = []string{"lock", "unlock", "renew"}
var metricStatuses = []string{"success", "conflict", "error"}

func ensureMetricsRegistered() {
	metricsRegisterOnce.Do(func() {
		lockOperationTotal = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricsNamespace,
				Subsystem: metricsSubsystem,
				Name:      "operations_total",
				Help:      "Total number of etcd lock operations by operation and status.",
			},
			[]string{"operation", "status"},
		)

		lockOperationDur = prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: metricsNamespace,
				Subsystem: metricsSubsystem,
				Name:      "operation_duration_seconds",
				Help:      "Latency of etcd lock operations by operation and status.",
				Buckets:   prometheus.DefBuckets,
			},
			[]string{"operation", "status"},
		)

		for _, operation := range metricOperations {
			for _, status := range metricStatuses {
				lockOperationTotal.WithLabelValues(operation, status)
				lockOperationDur.WithLabelValues(operation, status)
			}
		}

		prometheus.MustRegister(
			lockOperationTotal,
			lockOperationDur,
			prometheus.NewGaugeFunc(
				prometheus.GaugeOpts{
					Namespace: metricsNamespace,
					Subsystem: metricsSubsystem,
					Name:      "active_locks",
					Help:      "Current number of active etcd locks managed by the process.",
				},
				func() float64 {
					return float64(atomic.LoadInt64(&globalLockManager.stats.ActiveLocks))
				},
			),
			prometheus.NewCounterFunc(
				prometheus.CounterOpts{
					Namespace: metricsNamespace,
					Subsystem: metricsSubsystem,
					Name:      "total_locks",
					Help:      "Total number of locks ever added to the in-process renewal manager.",
				},
				func() float64 {
					return float64(atomic.LoadInt64(&globalLockManager.stats.TotalLocks))
				},
			),
			prometheus.NewCounterFunc(
				prometheus.CounterOpts{
					Namespace: metricsNamespace,
					Subsystem: metricsSubsystem,
					Name:      "renewal_attempts_total",
					Help:      "Total number of successful automatic renewal attempts.",
				},
				func() float64 {
					return float64(atomic.LoadInt64(&globalLockManager.stats.RenewalCount))
				},
			),
			prometheus.NewCounterFunc(
				prometheus.CounterOpts{
					Namespace: metricsNamespace,
					Subsystem: metricsSubsystem,
					Name:      "renewal_errors_total",
					Help:      "Total number of automatic renewal errors.",
				},
				func() float64 {
					return float64(atomic.LoadInt64(&globalLockManager.stats.RenewalErrors))
				},
			),
			prometheus.NewCounterFunc(
				prometheus.CounterOpts{
					Namespace: metricsNamespace,
					Subsystem: metricsSubsystem,
					Name:      "renewal_skipped_total",
					Help:      "Total number of skipped automatic renewal attempts due to worker pool saturation.",
				},
				func() float64 {
					return float64(atomic.LoadInt64(&globalLockManager.stats.SkippedRenewals))
				},
			),
		)
	})
}

func observeOperationLatency(operation string, status string, duration time.Duration) {
	ensureMetricsRegistered()
	lockOperationTotal.WithLabelValues(operation, status).Inc()
	lockOperationDur.WithLabelValues(operation, status).Observe(duration.Seconds())
}

func operationStatus(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, ErrLockAcquireConflict):
		return "conflict"
	default:
		return "error"
	}
}
