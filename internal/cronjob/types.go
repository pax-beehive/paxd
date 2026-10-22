// Package cronjob owns Pax schedules independently of agent-specific schedulers.
package cronjob

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound        = errors.New("cron job not found")
	ErrConflict        = errors.New("cron job changed or already has an active run")
	ErrInvalidArgument = errors.New("invalid cron job")
)

type Job struct {
	AgentID        string     `json:"agent_id"`
	RemoteID       string     `json:"remote_id"`
	CloudAPIURL    string     `json:"cloud_api_url"`
	ConnectionID   string     `json:"connection_id"`
	SessionMode    string     `json:"session_mode"`
	BoundSessionID string     `json:"bound_session_id,omitempty"`
	ID             string     `json:"id" gorm:"primaryKey"`
	Name           string     `json:"name"`
	Agent          string     `json:"agent"`
	Prompt         string     `json:"prompt"`
	WorkingDir     string     `json:"working_dir"`
	Schedule       string     `json:"schedule"`
	Timezone       string     `json:"timezone"`
	Enabled        bool       `json:"enabled"`
	TimeoutSeconds int        `json:"timeout_seconds"`
	NextRunAt      *time.Time `json:"next_run_at" gorm:"index"`
	ActiveRunID    string     `json:"active_run_id,omitempty"`
	Revision       uint64     `json:"revision"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (Job) TableName() string { return "cron_job" }

type CreateRequest struct {
	ConnectionID   string `json:"connection_id"`
	SessionMode    string `json:"session_mode,omitempty"`
	Name           string `json:"name"`
	Agent          string `json:"agent"`
	Prompt         string `json:"prompt"`
	WorkingDir     string `json:"working_dir"`
	Schedule       string `json:"schedule"`
	Timezone       string `json:"timezone"`
	Enabled        *bool  `json:"enabled,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type UpdateRequest struct {
	ConnectionID     *string `json:"connection_id,omitempty"`
	SessionMode      *string `json:"session_mode,omitempty"`
	ResetSession     bool    `json:"reset_session,omitempty"`
	Name             *string `json:"name,omitempty"`
	Agent            *string `json:"agent,omitempty"`
	Prompt           *string `json:"prompt,omitempty"`
	WorkingDir       *string `json:"working_dir,omitempty"`
	Schedule         *string `json:"schedule,omitempty"`
	Timezone         *string `json:"timezone,omitempty"`
	Enabled          *bool   `json:"enabled,omitempty"`
	TimeoutSeconds   *int    `json:"timeout_seconds,omitempty"`
	ExpectedRevision uint64  `json:"expected_revision"`
}

type Run struct {
	PublishedThroughID uint64     `json:"-"`
	SessionID          string     `json:"local_session_id,omitempty"`
	NativeSessionID    string     `json:"native_session_id,omitempty"`
	ManagerSessionID   string     `json:"session_id,omitempty"`
	Prompt             string     `json:"prompt"`
	WorkingDir         string     `json:"working_dir"`
	ConnectionID       string     `json:"connection_id"`
	SessionMode        string     `json:"session_mode"`
	ID                 string     `json:"id" gorm:"primaryKey"`
	JobID              string     `json:"job_id" gorm:"index"`
	Trigger            string     `json:"trigger"`
	Status             string     `json:"status" gorm:"index"`
	ScheduledAt        time.Time  `json:"scheduled_at"`
	StartedAt          *time.Time `json:"started_at,omitempty"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
	Output             string     `json:"output,omitempty"`
	Error              string     `json:"error,omitempty"`
	// Preserve the exact task accepted for execution when the job is edited.
	JobJSON string `json:"-"`
}

func (Run) TableName() string { return "cron_run" }

type Runner interface {
	Run(context.Context, *Job) (string, error)
}

type Options struct {
	Executor       SessionExecutor
	ValidateTarget func(context.Context, *Job) error
	Now            func() time.Time
	Runner         Runner
	LockPath       string
}

// SessionExecutor owns the native session lifecycle; Runner remains a unit-test seam.
type SessionExecutor interface {
	Execute(context.Context, *Job, *Run) (string, error)
	Sync(context.Context) error
}

type Session struct {
	ID           string    `json:"id" gorm:"primaryKey"`
	JobID        string    `json:"cronjob_id"`
	OriginRunID  string    `json:"origin_run_id"`
	Name         string    `json:"name"`
	ConnectionID string    `json:"connection_id"`
	RemoteID     string    `json:"remote_id"`
	CloudAPIURL  string    `json:"-"`
	AgentID      string    `json:"agent_id"`
	WorkingDir   string    `json:"working_dir"`
	NativeID     string    `json:"native_session_id"`
	ManagerID    string    `json:"session_id"`
	CreatedAt    time.Time `json:"created_at"`
}

func (Session) TableName() string { return "cron_session" }

type SessionEvent struct {
	ID        uint64 `gorm:"primaryKey;autoIncrement"`
	RunID     string `gorm:"index"`
	SessionID string `gorm:"index"`
	Direction string
	Payload   string
	CreatedAt time.Time
}

func (SessionEvent) TableName() string { return "cron_session_event" }

// Envelope preserves ciphertext across retries; equal record IDs must have equal bytes.
type Envelope struct {
	ID      string `gorm:"primaryKey"`
	Payload []byte
}

func (Envelope) TableName() string { return "cron_envelope" }
