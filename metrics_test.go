package etcdlock

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestMetricsRegistrationIsIdempotent(t *testing.T) {
	mustNotPanic(t, func() {
		ensureMetricsRegistered()
		ensureMetricsRegistered()
		_ = NewEtcdLockPlugin()
		_ = NewEtcdLockPlugin()
	})

	if findMetricFamily(t, "lynx_etcd_lock_operations_total") == nil {
		t.Fatal("expected operations_total metric family to be registered")
	}
	if findMetricFamily(t, "lynx_etcd_lock_active_locks") == nil {
		t.Fatal("expected active_locks metric family to be registered")
	}
}

func TestObserveOperationLatencyRecordsMetrics(t *testing.T) {
	ensureMetricsRegistered()

	beforeCounter := counterValue(t, "lynx_etcd_lock_operations_total", map[string]string{
		"operation": "lock",
		"status":    "success",
	})
	beforeHistogramCount := histogramCount(t, "lynx_etcd_lock_operation_duration_seconds", map[string]string{
		"operation": "lock",
		"status":    "success",
	})

	observeOperationLatency("lock", "success", 25*time.Millisecond)

	afterCounter := counterValue(t, "lynx_etcd_lock_operations_total", map[string]string{
		"operation": "lock",
		"status":    "success",
	})
	afterHistogramCount := histogramCount(t, "lynx_etcd_lock_operation_duration_seconds", map[string]string{
		"operation": "lock",
		"status":    "success",
	})

	if afterCounter != beforeCounter+1 {
		t.Fatalf("lock success counter delta = %v, want 1", afterCounter-beforeCounter)
	}
	if afterHistogramCount != beforeHistogramCount+1 {
		t.Fatalf("lock success histogram sample delta = %v, want 1", afterHistogramCount-beforeHistogramCount)
	}
}

func mustNotPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("unexpected panic: %v", recovered)
		}
	}()
	fn()
}

func findMetricFamily(t *testing.T, name string) any {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() == name {
			return family
		}
	}
	return nil
}

func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if labelsMatch(metric.GetLabel(), labels) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("metric %s with labels %v not found", name, labels)
	return 0
}

func histogramCount(t *testing.T, name string, labels map[string]string) uint64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if labelsMatch(metric.GetLabel(), labels) {
				return metric.GetHistogram().GetSampleCount()
			}
		}
	}
	t.Fatalf("histogram %s with labels %v not found", name, labels)
	return 0
}

func labelsMatch(pairs []*dto.LabelPair, labels map[string]string) bool {
	if len(pairs) != len(labels) {
		return false
	}
	for _, pair := range pairs {
		if labels[pair.GetName()] != pair.GetValue() {
			return false
		}
	}
	return true
}
