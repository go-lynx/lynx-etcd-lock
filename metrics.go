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

		// Register each collector individually so that re-importing this package
		// in a single test binary (multiple test packages linked together) does
		// not panic via MustRegister. On AlreadyRegisteredError we use the
		// previously registered instance to keep the active metric variables
		// pointing at the live collectors.
		if err := prometheus.DefaultRegisterer.Register(lockOperationTotal); err != nil {
			var are prometheus.AlreadyRegisteredError
			if !errors.As(err, &are) {
				panic(err)
			}
			if existing, ok := are.ExistingCollector.(*prometheus.CounterVec); ok {
				lockOperationTotal = existing
			}
		}
		if err := prometheus.DefaultRegisterer.Register(lockOperationDur); err != nil {
			var are prometheus.AlreadyRegisteredError
			if !errors.As(err, &are) {
				panic(err)
			}
			if existing, ok := are.ExistingCollector.(*prometheus.HistogramVec); ok {
				lockOperationDur = existing
			}
		}

		funcCollectors := []prometheus.Collector{
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
		}
		for _, c := range funcCollectors {
			if err := prometheus.DefaultRegisterer.Register(c); err != nil {
				var are prometheus.AlreadyRegisteredError
				if !errors.As(err, &are) {
					panic(err)
				}
			}
		}
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
