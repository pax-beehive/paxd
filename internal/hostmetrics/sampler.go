package hostmetrics

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/machineinfo"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
)

var ErrNoSample = errors.New("hostmetrics: no sample available")

type Collector func(ctx context.Context) (control.HostMetricsReport, error)

type Sampler struct {
	interval time.Duration
	collect  Collector
	identity control.HostMetricsReport

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

// WithIdentity overrides detected host identity fields while preserving the
// periodically sampled utilization metrics.
func (s *Sampler) WithIdentity(machineName, osName, arch string) *Sampler {
	if s == nil {
		return s
	}
	s.identity.MachineName = machineName
	s.identity.OS = osName
	s.identity.Arch = arch
	return s
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
	if s.identity.MachineName != "" {
		metrics.MachineName = s.identity.MachineName
	}
	if s.identity.OS != "" {
		metrics.OS = s.identity.OS
	}
	if s.identity.Arch != "" {
		metrics.Arch = s.identity.Arch
	}
	s.mu.Lock()
	s.latest = &metrics
	s.mu.Unlock()
}

func collectSystemMetrics(ctx context.Context) (control.HostMetricsReport, error) {
	_ = ctx
	metrics := control.HostMetricsReport{
		MachineName: machineinfo.Name(),
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
	}
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
