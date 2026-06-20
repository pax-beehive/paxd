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

	"github.com/pax-beehive/paxd/internal/acpclient"
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
	batchSize   int
}

// AgentRuntime binds one cloud agent to a local Hermes client.
type AgentRuntime struct {
	Agent              store.CloudAgent
	HermesClient       *hermes.Client
	SupportsHermesHTTP bool
	ACPSessionLister   *acpclient.SessionLister
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
		batchSize:   100,
	}
}

// WithBatchSize overrides the maximum number of sessions per status report.
func (c *Collector) WithBatchSize(batchSize int) *Collector {
	if batchSize > 0 {
		c.batchSize = batchSize
	}
	return c
}

// CollectAndReport gathers all session statuses and system metrics,
// then POSTs the report to the Cloud API.
func (c *Collector) CollectAndReport(ctx context.Context) error {
	agentStatuses := make([]cloud.AgentStatus, 0, len(c.agents))
	sessionCount := 0
	for _, runtime := range c.agents {
		var sessionStatuses []cloud.SessionStatus
		if runtime.SupportsHermesHTTP && runtime.HermesClient != nil {
			hermesSessions, err := runtime.HermesClient.GetSessions()
			if err == nil {
				for _, s := range hermesSessions {
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
			}
		}
		if len(sessionStatuses) == 0 && runtime.ACPSessionLister != nil {
			acpSessions, err := runtime.ACPSessionLister.List(ctx)
			if err == nil {
				for _, s := range acpSessions {
					sessionStatuses = append(sessionStatuses, cloud.SessionStatus{
						SessionID:     s.SessionID,
						AgentType:     firstNonEmpty(s.AgentType, runtime.Agent.AgentType),
						NativeID:      s.NativeID,
						Name:          s.Name,
						ProjectID:     s.ProjectID,
						Preview:       s.Preview,
						Status:        s.Status,
						CurrentTask:   s.CurrentTask,
						LastMessageAt: firstNonEmpty(s.UpdatedAt, s.LastActive),
					})
				}
			}
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

	baseReport := &cloud.NodeStatusReport{
		Hostname: c.hostname,
		Agents:   agentStatuses,
		System:   sys,
	}

	reports := splitReports(baseReport, c.batchSize)
	for i := range reports {
		if err := c.cloudClient.PostNodeStatus(reports[i]); err != nil {
			return fmt.Errorf("post status batch %d/%d: %w", i+1, len(reports), err)
		}
	}

	log.Printf(
		"[collector] reported %d agents, %d sessions in %d batch(es), cpu=%.1f%%, mem=%.1f%%",
		len(agentStatuses),
		sessionCount,
		len(reports),
		sys.CPUPercent,
		sys.MemoryPercent,
	)
	return nil
}

func splitReports(report *cloud.NodeStatusReport, batchSize int) []*cloud.NodeStatusReport {
	if batchSize <= 0 {
		batchSize = 100
	}
	var reports []*cloud.NodeStatusReport
	current := cloneReportWithoutAgents(report)
	currentSessions := 0

	flush := func() {
		if len(current.Agents) == 0 {
			return
		}
		reports = append(reports, current)
		current = cloneReportWithoutAgents(report)
		currentSessions = 0
	}

	for _, agent := range report.Agents {
		if len(agent.Sessions) == 0 {
			current.Agents = append(current.Agents, agent)
			continue
		}

		for start := 0; start < len(agent.Sessions); start += batchSize {
			end := start + batchSize
			if end > len(agent.Sessions) {
				end = len(agent.Sessions)
			}
			chunk := agent
			chunk.Sessions = append([]cloud.SessionStatus(nil), agent.Sessions[start:end]...)
			chunkSize := len(chunk.Sessions)
			if currentSessions > 0 && currentSessions+chunkSize > batchSize {
				flush()
			}
			current.Agents = append(current.Agents, chunk)
			currentSessions += chunkSize
			if currentSessions >= batchSize {
				flush()
			}
		}
	}
	flush()
	if len(reports) == 0 {
		reports = append(reports, cloneReportWithoutAgents(report))
	}
	return reports
}

func cloneReportWithoutAgents(report *cloud.NodeStatusReport) *cloud.NodeStatusReport {
	return &cloud.NodeStatusReport{
		NodeID:   report.NodeID,
		Hostname: report.Hostname,
		System:   report.System,
		Metadata: report.Metadata,
	}
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
