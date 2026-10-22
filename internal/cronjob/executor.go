package cronjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pax-beehive/paxd/internal/auth"
	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/e2ee"
	"github.com/pax-beehive/paxd/internal/runtime"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ACPExecutor runs inside paxd and shares ordinary sessions' routing and E2EE.
type ACPExecutor struct {
	Store    *daemonstore.Store
	Pools    *runtime.ACPPoolRegistry
	Headers  auth.HeaderProvider
	RootKeys e2ee.RootKeyProvider
	syncMu   sync.Mutex
}

func (e *ACPExecutor) target(ctx context.Context, id string) (*runtime.AgentConnectionSpec, error) {
	connections, err := e.Store.ListDesiredAgentConnections(ctx)
	if err != nil {
		return nil, err
	}
	for _, connection := range connections {
		if connection.ConnectionID == id && connection.CloudAgentID != "" {
			return &connection, nil
		}
	}
	return nil, errors.New("connection must be enabled and registered with Manager")
}

func (e *ACPExecutor) ValidateTarget(ctx context.Context, job *Job) error {
	target, err := e.target(ctx, job.ConnectionID)
	if err != nil {
		return err
	}
	job.Agent = target.AgentType
	job.AgentID, job.RemoteID, job.CloudAPIURL = target.CloudAgentID, target.RemoteID, target.CloudAPIURL
	return nil
}

func (e *ACPExecutor) Execute(ctx context.Context, job *Job, run *Run) (string, error) {
	target, err := e.target(ctx, job.ConnectionID)
	if err != nil {
		return "", err
	}
	if target.CloudAgentID != job.AgentID || target.RemoteID != job.RemoteID || target.CloudAPIURL != job.CloudAPIURL {
		return "", errors.New("configured agent connection no longer matches the saved workspace")
	}
	pool, err := e.Pools.Get(job.ConnectionID)
	if err != nil {
		return "", err
	}
	client := runtime.NewLocalSessionClient(pool)
	if err = client.WaitReady(ctx); err != nil {
		return "", err
	}
	session, fresh, err := reserveSession(ctx, e.Store.DB(), job, run, target)
	if err != nil {
		return "", err
	}
	if fresh {
		created, createErr := client.CreateSession(ctx, session.WorkingDir)
		if createErr != nil {
			return "", createErr
		}
		session.NativeID = created.NativeID
		// Preserve the result even if the execution deadline expired concurrently.
		persistCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = e.Store.DB().WithContext(persistCtx).Model(session).Update("native_id", session.NativeID).Error
		cancel()
		if err != nil {
			return "", err
		}
	} else {
		if session.NativeID == "" {
			return "", errors.New("previous session creation has an unknown outcome; reset the session binding before retrying")
		}
		if err = client.ResumeSession(ctx, session.NativeID, session.WorkingDir); err != nil {
			return "", err
		}
	}
	if err = e.Store.DB().WithContext(ctx).Model(run).Update("native_session_id", session.NativeID).Error; err != nil {
		return "", err
	}
	// Registration is independently retried from durable state. An unavailable
	// Manager does not discard a locally executable task or its transcript.
	registerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	e.syncMu.Lock()
	_ = e.register(registerCtx, session)
	e.syncMu.Unlock()
	cancel()
	record := func(direction string) func(context.Context, []byte) error {
		return func(_ context.Context, payload []byte) error {
			persistCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return e.Store.DB().WithContext(persistCtx).Create(&SessionEvent{RunID: run.ID, SessionID: session.ID, Direction: direction, Payload: string(payload), CreatedAt: time.Now().UTC()}).Error
		}
	}
	release, err := client.ObserveSession(session.NativeID, record("output"))
	if err != nil {
		return "", err
	}
	defer release()
	result, err := client.Prompt(ctx, session.NativeID, job.Prompt, record("input"))
	return string(result), err
}

// Reserve before session/new: a crash during creation must never silently create
// a second session for a reuse binding. The creation run remains its origin.
func reserveSession(ctx context.Context, db *gorm.DB, job *Job, run *Run, target *runtime.AgentConnectionSpec) (*Session, bool, error) {
	var session Session
	fresh := false
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Job
		if err := tx.First(&current, "id = ?", job.ID).Error; err != nil {
			return err
		}
		if job.SessionMode == "reuse" && current.BoundSessionID != "" {
			if err := tx.First(&session, "id = ?", current.BoundSessionID).Error; err != nil {
				return err
			}
			if session.ConnectionID != job.ConnectionID || session.WorkingDir != job.WorkingDir || session.AgentID != target.CloudAgentID || session.RemoteID != target.RemoteID || session.CloudAPIURL != target.CloudAPIURL {
				return errors.New("bound session workspace or agent changed; reset the session binding")
			}
		} else {
			id, err := newID("cronsess_")
			if err != nil {
				return err
			}
			session = Session{ID: id, JobID: job.ID, OriginRunID: run.ID, Name: job.Name, ConnectionID: job.ConnectionID, RemoteID: target.RemoteID, CloudAPIURL: target.CloudAPIURL, AgentID: target.CloudAgentID, WorkingDir: job.WorkingDir, CreatedAt: time.Now().UTC()}
			if err = tx.Create(&session).Error; err != nil {
				return err
			}
			if job.SessionMode == "reuse" {
				if err = tx.Model(&Job{}).Where("id = ?", job.ID).Updates(map[string]any{"bound_session_id": session.ID, "revision": gorm.Expr("revision + 1")}).Error; err != nil {
					return err
				}
			}
			fresh = true
		}
		run.SessionID = session.ID
		return tx.Model(run).Updates(map[string]any{"session_id": session.ID, "native_session_id": session.NativeID, "manager_session_id": session.ManagerID}).Error
	})
	return &session, fresh, err
}

func (e *ACPExecutor) register(ctx context.Context, session *Session) error {
	if session.NativeID == "" {
		return nil
	}
	if session.ManagerID == "" {
		headers, err := e.Headers.Headers(ctx, session.RemoteID)
		if err != nil {
			return err
		}
		client := cloud.NewClient(session.CloudAPIURL, headers.Get(auth.HeaderPaxKey))
		registered, err := client.RegisterSession(ctx, session.AgentID, &cloud.RegisterSessionRequest{NativeID: session.NativeID, CWD: session.WorkingDir, Origin: cloud.SessionOrigin{Kind: "cronjob", RegistrationID: session.ID, CronjobID: session.JobID, RunID: session.OriginRunID, Name: session.Name}})
		if err != nil {
			return err
		}
		if registered.SessionID == "" {
			return errors.New("Manager returned an empty session ID")
		}
		session.ManagerID = registered.SessionID
		if err = e.Store.DB().WithContext(ctx).Model(session).Update("manager_id", session.ManagerID).Error; err != nil {
			return err
		}
	}
	_, err := e.Store.BindACPSessionRouteManagerID(ctx, daemonstore.ACPSessionManagerBinding{ConnectionID: session.ConnectionID, NativeSessionID: session.NativeID, ManagerSessionID: session.ManagerID})
	if err != nil {
		return err
	}
	return e.Store.DB().WithContext(ctx).Model(&Run{}).Where("session_id = ?", session.ID).Update("manager_session_id", session.ManagerID).Error
}

func (e *ACPExecutor) Sync(ctx context.Context) error {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	var sessions []Session
	// Include registered sessions with unpublished events, to recover a crash
	// between registration, route binding, and transcript publication.
	query := e.Store.DB().WithContext(ctx).Where("native_id <> '' AND (manager_id = '' OR id IN (SELECT session_id FROM cron_run WHERE EXISTS (SELECT 1 FROM cron_session_event WHERE cron_session_event.run_id = cron_run.id AND cron_session_event.id > cron_run.published_through_id)))")
	if err := query.Find(&sessions).Error; err != nil {
		return err
	}
	var failures []error
	for i := range sessions {
		session := &sessions[i]
		syncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := e.register(syncCtx, session)
		if err == nil {
			err = e.publish(syncCtx, session)
		}
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("session %s: %w", session.ID, err))
		}
	}
	return errors.Join(failures...)
}

func (e *ACPExecutor) publish(ctx context.Context, session *Session) error {
	pool, err := e.Pools.Get(session.ConnectionID)
	if err != nil {
		return err
	}
	var runs []Run
	if err = e.Store.DB().WithContext(ctx).Where("session_id = ? AND EXISTS (SELECT 1 FROM cron_session_event WHERE cron_session_event.run_id = cron_run.id AND cron_session_event.id > cron_run.published_through_id)", session.ID).Find(&runs).Error; err != nil {
		return err
	}
	for _, run := range runs {
		var events []SessionEvent
		if err = e.Store.DB().WithContext(ctx).Where("run_id = ?", run.ID).Order("id ASC").Find(&events).Error; err != nil {
			return err
		}
		frames := make([]runtime.LocalHistoryFrame, 0, len(events))
		for _, event := range events {
			frames = append(frames, runtime.LocalHistoryFrame{ID: event.ID, Direction: event.Direction, Payload: json.RawMessage(event.Payload), CreatedAt: event.CreatedAt})
		}
		if len(frames) == 0 {
			continue
		}
		if err = pool.PublishLocalHistory(ctx, session.AgentID, session.ManagerID, run.ID, frames, run.PublishedThroughID, e.RootKeys, e.persistEnvelope); err != nil {
			return err
		}
		if err = e.Store.DB().WithContext(ctx).Model(&Run{}).Where("id = ?", run.ID).Update("published_through_id", frames[len(frames)-1].ID).Error; err != nil {
			return err
		}
	}
	return nil
}

func (e *ACPExecutor) persistEnvelope(ctx context.Context, id string, data []byte) ([]byte, error) {
	row := Envelope{ID: id, Payload: data}
	if err := e.Store.DB().WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return nil, err
	}
	if err := e.Store.DB().WithContext(ctx).First(&row, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return row.Payload, nil
}
