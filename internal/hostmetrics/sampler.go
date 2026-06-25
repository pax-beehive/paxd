package hostmetrics

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
)

var ErrNoSample = errors.New("hostmetrics: no sample available")

type Collector func(ctx context.Context) (control.HostMetricsReport, error)

type Sampler struct {
	interval time.Duration
	collect  Collector

	mu     sync.RWMutex
	latest *control.HostMetricsReport
}

func NewSampler(interval time.Duration) *Sampler {
	return NewSamplerWithCollector(interval, collectSystemMetrics)
}

func NewSamplerWithCollector(interval time.Duration, collect Collector) *Sampler {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if collect == nil {
		collect = collectSystemMetrics
	}
	return &Sampler{interval: interval, collect: collect}
}

func (s *Sampler) Start(ctx context.Context) {
	if s == nil {
		return
	}
	go func() {
		s.sample(ctx)
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sample(ctx)
			}
		}
	}()
}

func (s *Sampler) CurrentHostMetrics(ctx context.Context) (*control.HostMetricsReport, error) {
	_ = ctx
	if s == nil {
		return nil, ErrNoSample
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.latest == nil {
		return nil, ErrNoSample
	}
	copy := *s.latest
	return &copy, nil
}

func (s *Sampler) sample(ctx context.Context) {
	if s == nil || s.collect == nil {
		return
	}
	metrics, err := s.collect(ctx)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.latest = &metrics
	s.mu.Unlock()
}

func collectSystemMetrics(ctx context.Context) (control.HostMetricsReport, error) {
	_ = ctx
	var metrics control.HostMetricsReport
	var firstErr error
	if cpuPercent, err := cpu.Percent(time.Second, false); err == nil && len(cpuPercent) > 0 {
		metrics.CPUPercent = cpuPercent[0]
	} else if err != nil {
		firstErr = err
	}
	if memInfo, err := mem.VirtualMemory(); err == nil {
		metrics.MemoryPercent = memInfo.UsedPercent
	} else if firstErr == nil {
		firstErr = err
	}
	if hostInfo, err := host.Info(); err == nil {
		metrics.UptimeSeconds = int64(hostInfo.Uptime)
	} else if firstErr == nil {
		firstErr = err
	}
	metrics.CollectedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if firstErr != nil && metrics.CPUPercent == 0 && metrics.MemoryPercent == 0 && metrics.UptimeSeconds == 0 {
		return control.HostMetricsReport{}, firstErr
	}
	return metrics, nil
}
