package hostmetrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
)

func TestSamplerCachesLatestMetrics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sampler := NewSamplerWithCollector(time.Hour, func(context.Context) (control.HostMetricsReport, error) {
		return control.HostMetricsReport{
			CPUPercent:    11.5,
			MemoryPercent: 44.5,
			UptimeSeconds: 99,
			CollectedAt:   "2026-06-24T12:00:00Z",
		}, nil
	})

	sampler.Start(ctx)

	var got *control.HostMetricsReport
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err = sampler.CurrentHostMetrics(ctx)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("CurrentHostMetrics() error = %v", err)
	}
	if got.CPUPercent != 11.5 || got.MemoryPercent != 44.5 || got.UptimeSeconds != 99 {
		t.Fatalf("metrics = %+v", got)
	}
}

func TestSamplerAppliesConfiguredHostIdentity(t *testing.T) {
	sampler := NewSamplerWithCollector(time.Hour, func(context.Context) (control.HostMetricsReport, error) {
		return control.HostMetricsReport{MachineName: "detected", OS: "detected", Arch: "detected"}, nil
	}).WithIdentity("MacBook Pro", "darwin", "amd64")

	sampler.sample(context.Background())
	got, err := sampler.CurrentHostMetrics(context.Background())
	if err != nil {
		t.Fatalf("CurrentHostMetrics() error = %v", err)
	}
	if got.MachineName != "MacBook Pro" || got.OS != "darwin" || got.Arch != "amd64" {
		t.Fatalf("identity = %+v", got)
	}
}

func TestSamplerKeepsPreviousSampleAfterCollectionFailure(t *testing.T) {
	ctx := context.Background()
	calls := 0
	sampler := NewSamplerWithCollector(time.Hour, func(context.Context) (control.HostMetricsReport, error) {
		calls++
		if calls == 1 {
			return control.HostMetricsReport{CPUPercent: 10, CollectedAt: "first"}, nil
		}
		return control.HostMetricsReport{}, errors.New("sensor failed")
	})

	sampler.sample(ctx)
	sampler.sample(ctx)

	got, err := sampler.CurrentHostMetrics(ctx)
	if err != nil {
		t.Fatalf("CurrentHostMetrics() error = %v", err)
	}
	if got.CPUPercent != 10 || got.CollectedAt != "first" {
		t.Fatalf("metrics = %+v, want previous sample", got)
	}
}

func TestSamplerReturnsNoSampleBeforeCollectionSucceeds(t *testing.T) {
	sampler := NewSamplerWithCollector(time.Hour, func(context.Context) (control.HostMetricsReport, error) {
		return control.HostMetricsReport{}, errors.New("sensor failed")
	})

	sampler.sample(context.Background())
	got, err := sampler.CurrentHostMetrics(context.Background())

	if !errors.Is(err, ErrNoSample) {
		t.Fatalf("CurrentHostMetrics() error = %v, want ErrNoSample", err)
	}
	if got != nil {
		t.Fatalf("metrics = %+v, want nil", got)
	}
}

func TestNewSamplerUsesDefaultCollector(t *testing.T) {
	sampler := NewSampler(time.Hour)
	if sampler == nil {
		t.Fatal("NewSampler() returned nil")
	}
	if sampler.interval != time.Hour {
		t.Fatalf("interval = %s, want time.Hour", sampler.interval)
	}

	defaultInterval := NewSamplerWithCollector(0, func(context.Context) (control.HostMetricsReport, error) {
		return control.HostMetricsReport{}, nil
	})
	if defaultInterval.interval != 10*time.Second {
		t.Fatalf("default interval = %s, want 10s", defaultInterval.interval)
	}

	defaultCollector := NewSamplerWithCollector(time.Hour, nil)
	if defaultCollector.collect == nil {
		t.Fatal("default collector is nil")
	}
}

func TestNilSamplerIsSafe(t *testing.T) {
	var sampler *Sampler
	sampler.Start(context.Background())
	sampler.sample(context.Background())
	got, err := sampler.CurrentHostMetrics(context.Background())
	if !errors.Is(err, ErrNoSample) {
		t.Fatalf("CurrentHostMetrics() error = %v, want ErrNoSample", err)
	}
	if got != nil {
		t.Fatalf("metrics = %+v, want nil", got)
	}
}

func TestCollectSystemMetricsReturnsBestEffortSample(t *testing.T) {
	metrics, err := collectSystemMetrics(context.Background())
	if err != nil {
		t.Fatalf("collectSystemMetrics() error = %v", err)
	}
	if metrics.CollectedAt == "" {
		t.Fatalf("metrics = %+v, want collected_at", metrics)
	}
	if metrics.MachineName == "" || metrics.OS == "" || metrics.Arch == "" {
		t.Fatalf("metrics = %+v, want host identity", metrics)
	}
	if metrics.CPUPercent < 0 || metrics.MemoryPercent < 0 || metrics.UptimeSeconds < 0 {
		t.Fatalf("metrics = %+v, want non-negative values", metrics)
	}
}
