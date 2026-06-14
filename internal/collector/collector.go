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
	agents      []AgentRuntime
	cloudClient *cloud.Client
	store       *store.Store
	hostname    string
}

// AgentRuntime binds one cloud agent to a local Hermes client.
type AgentRuntime struct {
	Agent        store.CloudAgent
	HermesClient *hermes.Client
}

// New creates a new Collector.
func New(
	agents []AgentRuntime,
	cloudClient *cloud.Client,
	s *store.Store,
	hostname string,
) *Collector {
	return &Collector{
		agents:      agents,
		cloudClient: cloudClient,
		store:       s,
		hostname:    hostname,
	}
}

// CollectAndReport gathers all session statuses and system metrics,
// then POSTs the report to the Cloud API.
func (c *Collector) CollectAndReport(ctx context.Context) error {
	agentStatuses := make([]cloud.AgentStatus, 0, len(c.agents))
	sessionCount := 0
	for _, runtime := range c.agents {
		sessions, err := runtime.HermesClient.GetSessions()
		if err != nil {
			log.Printf(
				"[collector] get sessions for agent %s (non-fatal): %v",
				runtime.Agent.AgentID,
				err,
			)
			sessions = nil
		}

		var sessionStatuses []cloud.SessionStatus
		for _, s := range sessions {
			sessionStatuses = append(sessionStatuses, cloud.SessionStatus{
				SessionID:     s.SessionID,
				AgentType:     firstNonEmpty(s.AgentType, runtime.Agent.AgentType),
				NativeID:      s.NativeID,
				Name:          s.Name,
				ProjectID:     s.ProjectID,
				Preview:       s.Preview,
				Status:        s.Status,
				CurrentTask:   s.CurrentTask,
				TokenUsage:    s.TokenUsage,
				LastMessageAt: firstNonEmpty(s.UpdatedAt, s.LastActive),
			})
		}
		sessionCount += len(sessionStatuses)
		agentStatuses = append(agentStatuses, cloud.AgentStatus{
			AgentID:   runtime.Agent.AgentID,
			Name:      runtime.Agent.Name,
			AgentType: firstNonEmpty(runtime.Agent.AgentType, "hermes"),
			Status:    "online",
			Online:    true,
			Sessions:  sessionStatuses,
		})
	}

	// Collect system metrics
	sys, err := collectSystemMetrics()
	if err != nil {
		log.Printf("[collector] system metrics error (non-fatal): %v", err)
		sys = cloud.SystemMetrics{} // send what we can
	}

	report := &cloud.NodeStatusReport{
		Hostname: c.hostname,
		Agents:   agentStatuses,
		System:   sys,
	}

	if err := c.cloudClient.PostNodeStatus(report); err != nil {
		return fmt.Errorf("post status: %w", err)
	}

	log.Printf(
		"[collector] reported %d agents, %d sessions, cpu=%.1f%%, mem=%.1f%%",
		len(agentStatuses),
		sessionCount,
		sys.CPUPercent,
		sys.MemoryPercent,
	)
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
