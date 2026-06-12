// Package collector gathers local Hermes session status and system metrics,
// then reports them to the Fleet Cloud API.
package collector

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/hermes"
	"github.com/pax-beehive/paxd/internal/store"
)

// Collector gathers and reports agent status.
type Collector struct {
	hermesClient *hermes.Client
	cloudClient  *cloud.Client
	store        *store.Store
	agentID      string
}

// New creates a new Collector.
func New(hermesClient *hermes.Client, cloudClient *cloud.Client, s *store.Store, agentID string) *Collector {
	return &Collector{
		hermesClient: hermesClient,
		cloudClient:  cloudClient,
		store:        s,
		agentID:      agentID,
	}
}

// CollectAndReport gathers all session statuses and system metrics,
// then POSTs the report to the Cloud API.
func (c *Collector) CollectAndReport(ctx context.Context) error {
	// Collect Hermes sessions
	sessions, err := c.hermesClient.GetSessions()
	if err != nil {
		return fmt.Errorf("get sessions: %w", err)
	}

	var sessionStatuses []cloud.SessionStatus
	for _, s := range sessions {
		sessionStatuses = append(sessionStatuses, cloud.SessionStatus{
			SessionID:    s.SessionID,
			Status:       s.Status,
			CurrentTask:  s.CurrentTask,
			TokenUsage:   s.TokenUsage,
			LastActiveAt: s.UpdatedAt,
		})
	}

	// Collect system metrics
	sys, err := collectSystemMetrics()
	if err != nil {
		log.Printf("[collector] system metrics error (non-fatal): %v", err)
		sys = cloud.SystemMetrics{} // send what we can
	}

	report := &cloud.StatusReport{
		AgentID:  c.agentID,
		Status:   "online",
		Sessions: sessionStatuses,
		System:   sys,
	}

	if err := c.cloudClient.PostStatus(report); err != nil {
		return fmt.Errorf("post status: %w", err)
	}

	log.Printf("[collector] reported %d sessions, cpu=%.1f%%, mem=%.1f%%",
		len(sessionStatuses), sys.CPUPercent, sys.MemoryPercent)
	return nil
}

// collectSystemMetrics gathers CPU, memory, and uptime.
func collectSystemMetrics() (cloud.SystemMetrics, error) {
	var m cloud.SystemMetrics

	// CPU
	cpuPercent, err := cpu.Percent(time.Second, false)
	if err == nil && len(cpuPercent) > 0 {
		m.CPUPercent = cpuPercent[0]
	}

	// Memory
	memInfo, err := mem.VirtualMemory()
	if err == nil {
		m.MemoryPercent = memInfo.UsedPercent
	}

	// Uptime
	uptime, err := host.Uptime()
	if err == nil {
		m.UptimeSeconds = int64(uptime)
	}

	return m, nil
}
