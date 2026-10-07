package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/secretchannel"
)

var (
	ErrNotFound  = errors.New("control: not found")
	ErrDuplicate = errors.New("control: duplicate")
)

const (
	ErrCodeNotFound = "not_found"
	ErrCodeConflict = "conflict"
	ErrCodeInternal = "internal_error"
)

type Store interface {
	WithTx(ctx context.Context, fn func(TxStore) error) error
	GetCommandRecord(ctx context.Context, commandID string) (*CommandRecord, error)
	CompleteCommand(ctx context.Context, commandID string, completion CommandCompletion) error
	StoreReader
}

type TxStore interface {
	InsertCommand(ctx context.Context, rec CommandRecord) error
	CreateRemote(ctx context.Context, cmd CreateRemoteCommand) (RemoteView, error)
	UpdateRemote(ctx context.Context, cmd UpdateRemoteCommand) (RemoteView, error)
	DeleteRemote(ctx context.Context, cmd DeleteRemoteCommand) (RemoteView, error)
	RestartRemote(ctx context.Context, cmd RestartRemoteCommand) (RemoteView, error)
	ConfigureRemoteAuth(ctx context.Context, cmd ConfigureRemoteAuthCommand) error
	ClearRemoteAuth(ctx context.Context, cmd ClearRemoteAuthCommand) error
	CreateAgentConnection(ctx context.Context, cmd CreateAgentConnectionCommand) (AgentConnectionView, error)
	UpdateAgentConnection(ctx context.Context, cmd UpdateAgentConnectionCommand) (AgentConnectionView, error)
	DeleteAgentConnection(ctx context.Context, cmd DeleteAgentConnectionCommand) (AgentConnectionView, error)
	RestartAgentConnection(ctx context.Context, cmd RestartAgentConnectionCommand) (AgentConnectionView, error)
}

type StoreReader interface {
	GetCommand(ctx context.Context, commandID string) (*CommandView, error)
	ListRemotes(ctx context.Context, filter ListRemotesQuery) ([]RemoteView, error)
	ListAgentConnections(ctx context.Context, filter ListAgentConnectionsQuery) ([]AgentConnectionView, error)
}

type CommandRecord struct {
	CommandID         string
	Source            Source
	Type              CommandType
	TargetType        string
	TargetID          string
	PayloadJSON       string
	Status            CommandStatus
	DesiredGeneration *int64
	ErrorCode         string
	ErrorMessage      string
	ResultJSON        string
}

type CommandCompletion struct {
	Status            CommandStatus
	DesiredGeneration *int64
	ErrorCode         string
	ErrorMessage      string
	ResultJSON        string
}

type Supervisors interface {
	WakeRemotes()
	WakeAgentConnections()
	WakeACPSlots()
}

type HarnessRegistry interface {
	ListCached(ctx context.Context) ([]HarnessView, error)
	Discover(ctx context.Context, req DiscoverHarnessesQuery) ([]HarnessView, error)
}

type LocalSessions interface {
	List(ctx context.Context, query ListLocalSessionsQuery) ([]LocalSessionView, error)
	Sync(ctx context.Context, query SyncLocalSessionsQuery) (LocalSessionSyncResult, error)
	Get(ctx context.Context, query GetLocalSessionQuery) (*LocalSessionView, error)
}

// SecretChannel is the narrow port onto internal/secretchannel.Registry.
// It is deliberately not persisted through Store: a paxd restart is
// meant to invalidate every outstanding channel (see the package doc).
type SecretChannel interface {
	Open(identity secretchannel.Identity) (secretchannel.ChannelInfo, error)
	Consume(identity secretchannel.Identity, req secretchannel.PushRequest) (secretchannel.PushResult, error)
}

type ServiceOptions struct {
	HarnessAuth         HarnessAuth
	Store               Store
	Supervisors         Supervisors
	Harnesses           HarnessRegistry
	LocalSessions       LocalSessions
	HostMetrics         HostMetricsProvider
	ACPPoolCapabilities ACPPoolCapabilitySource
	Diagnostics         DiagnosticsProvider
	Attachments         AttachmentLocalizer
	SessionRuntime      SessionRuntimeReportService
	SessionRuntimeReset SessionRuntimeResetService
	PaxdLifecycle       PaxdLifecycle
	BrowserControl      BrowserControl
	SecretChannel       SecretChannel
}

type ControlService struct {
	harnessAuth         HarnessAuth
	store               Store
	supervisors         Supervisors
	harnesses           HarnessRegistry
	localSessions       LocalSessions
	hostMetrics         HostMetricsProvider
	acpPoolCapabilities ACPPoolCapabilitySource
	diagnostics         DiagnosticsProvider
	attachments         AttachmentLocalizer
	sessionRuntime      SessionRuntimeReportService
	sessionRuntimeReset SessionRuntimeResetService
	paxdLifecycle       PaxdLifecycle
	browserControl      BrowserControl
	secretChannel       SecretChannel
}

func NewService(opts ServiceOptions) *ControlService {
	return &ControlService{
		harnessAuth:         opts.HarnessAuth,
		store:               opts.Store,
		supervisors:         opts.Supervisors,
		harnesses:           opts.Harnesses,
		localSessions:       opts.LocalSessions,
		hostMetrics:         opts.HostMetrics,
		acpPoolCapabilities: opts.ACPPoolCapabilities,
		diagnostics:         opts.Diagnostics,
		attachments:         opts.Attachments,
		sessionRuntime:      opts.SessionRuntime,
		sessionRuntimeReset: opts.SessionRuntimeReset,
		paxdLifecycle:       opts.PaxdLifecycle,
		secretChannel:       opts.SecretChannel,
		browserControl:      opts.BrowserControl,
	}
}

func (s *ControlService) BuildSessionRuntimeSnapshots(
	ctx context.Context,
	remoteID string,
	nodeID string,
) ([]SessionRuntimeSnapshotReport, error) {
	if s.sessionRuntime == nil {
		return nil, nil
	}
	return s.sessionRuntime.BuildSessionRuntimeSnapshots(ctx, remoteID, nodeID)
}

func (s *ControlService) SubscribeSessionRuntime(remoteID string) (<-chan struct{}, func()) {
	if s.sessionRuntime == nil {
		return nil, func() {}
	}
	return s.sessionRuntime.SubscribeSessionRuntime(remoteID)
}

func (s *ControlService) HandleCommand(ctx context.Context, src Source, cmd Command) (CommandAck, error) {
	if err := cmd.Validate(); err != nil {
		return rejectedAck(cmd.CommandID, "", "", controlErr(err)), nil
	}
	if cmd.Type == CommandHarnessAuthLogin {
		return s.handleHarnessAuthLogin(ctx, src, cmd)
	}
	if cmd.Type == CommandSessionRuntimeReset {
		return s.handleSessionRuntimeReset(ctx, src, cmd)
	}
	if cmd.Type == CommandSecretChannelPush {
		return s.handleSecretChannelPush(ctx, src, cmd)
	}
	if isPaxdMaintenanceCommand(cmd.Type) {
		if src.Kind != SourceRemote || strings.TrimSpace(src.RemoteID) == "" {
			return rejectedAck(cmd.CommandID, "paxd", "", ControlError{
				Code: ErrCodeInvalidArgument, Message: "paxd maintenance requires an authenticated remote node-control source",
			}), nil
		}
		if s.paxdLifecycle == nil {
			return failedAck(cmd.CommandID, "paxd", "", ControlError{
				Code: ErrCodeInternal, Message: "paxd lifecycle is not configured",
			}), nil
		}
	}
	if s.store == nil {
		return failedAck(cmd.CommandID, "", "", ControlError{Code: ErrCodeInternal, Message: "control store is not configured"}), nil
	}

	payloadJSON, err := commandPayloadJSON(cmd)
	if err != nil {
		return failedAck(cmd.CommandID, "", "", ControlError{Code: ErrCodeInternal, Message: "failed to encode command payload"}), nil
	}

	existing, err := s.store.GetCommandRecord(ctx, cmd.CommandID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return failedAck(cmd.CommandID, "", "", errorToControlError(err)), nil
	}
	if existing != nil {
		if existing.Type != cmd.Type || existing.PayloadJSON != payloadJSON {
			return rejectedAck(cmd.CommandID, existing.TargetType, existing.TargetID, ControlError{
				Code:    ErrCodeConflict,
				Message: "command id already exists with a different payload",
				Target:  "command_id",
			}), nil
		}
		if isSourceBoundCommand(cmd.Type) && !sameCommandSource(existing.Source, src) {
			return rejectedAck(cmd.CommandID, existing.TargetType, existing.TargetID, ControlError{
				Code:    ErrCodeConflict,
				Message: "command id belongs to a different remote source",
				Target:  "command_id",
			}), nil
		}
		ack := ackFromRecord(*existing)
		if isDeferredPaxdMaintenance(cmd.Type) {
			return s.prepareMaintenanceAck(ctx, cmd, *existing, ack), nil
		}
		return ack, nil
	}

	targetType, targetID := commandTarget(cmd)
	rec := CommandRecord{
		CommandID:   cmd.CommandID,
		Source:      src,
		Type:        cmd.Type,
		TargetType:  targetType,
		TargetID:    targetID,
		PayloadJSON: payloadJSON,
		Status:      CommandStatusReceived,
	}

	var ack CommandAck
	var wakeRemotes, wakeAgents bool
	err = s.store.WithTx(ctx, func(tx TxStore) error {
		if err := tx.InsertCommand(ctx, rec); err != nil {
			return err
		}
		var mutationErr error
		ack, wakeRemotes, wakeAgents, mutationErr = s.applyCommand(ctx, tx, src, cmd)
		return mutationErr
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrDuplicate) {
			return rejectedAck(cmd.CommandID, targetType, targetID, errorToControlError(err)), nil
		}
		return failedAck(cmd.CommandID, targetType, targetID, errorToControlError(err)), nil
	}
	if isDeferredPaxdMaintenance(cmd.Type) {
		rec, lookupErr := s.store.GetCommandRecord(ctx, cmd.CommandID)
		if lookupErr != nil {
			return failedAck(cmd.CommandID, "paxd", "", errorToControlError(lookupErr)), nil
		}
		ack = s.prepareMaintenanceAck(ctx, cmd, *rec, ack)
	}
	if cmd.Type == CommandCancelPaxdMaintenance {
		if cancelErr := s.paxdLifecycle.Cancel(cmd.CancelPaxdMaintenance.MaintenanceCommandID); cancelErr != nil {
			controlErr := errorToControlError(cancelErr)
			_ = s.store.CompleteCommand(ctx, cmd.CommandID, CommandCompletion{Status: CommandStatusFailed, ErrorCode: controlErr.Code, ErrorMessage: cancelErr.Error()})
			return failedAck(cmd.CommandID, "paxd", cmd.CancelPaxdMaintenance.MaintenanceCommandID, controlErr), nil
		}
		_ = s.store.CompleteCommand(ctx, cmd.CommandID, CommandCompletion{Status: CommandStatusApplied})
		ack.Status = CommandStatusApplied
	}
	wakeACPSlots := wakeAgents
	if isDesiredSlotsOnlyUpdate(cmd) {
		wakeAgents = false
	}

	if wakeRemotes {
		if s.supervisors != nil {
			log.Printf("[paxd] control command=%s type=%s waking remote supervisor", cmd.CommandID, cmd.Type)
			s.supervisors.WakeRemotes()
		} else {
			log.Printf("[paxd] control command=%s type=%s needs remote reconcile but no supervisors are configured", cmd.CommandID, cmd.Type)
		}
	}
	if wakeAgents {
		if s.supervisors != nil {
			log.Printf("[paxd] control command=%s type=%s waking agent connection supervisor", cmd.CommandID, cmd.Type)
			s.supervisors.WakeAgentConnections()
		} else {
			log.Printf("[paxd] control command=%s type=%s needs agent connection reconcile but no supervisors are configured", cmd.CommandID, cmd.Type)
		}
	}
	if wakeACPSlots {
		if s.supervisors != nil {
			log.Printf("[paxd] control command=%s type=%s waking acp slot supervisor", cmd.CommandID, cmd.Type)
			s.supervisors.WakeACPSlots()
		} else {
			log.Printf("[paxd] control command=%s type=%s needs acp slot reconcile but no supervisors are configured", cmd.CommandID, cmd.Type)
		}
	}
	return ack, nil
}

func sameCommandSource(left Source, right Source) bool {
	return left.Kind == right.Kind &&
		strings.TrimSpace(left.RemoteID) == strings.TrimSpace(right.RemoteID)
}

func isSourceBoundCommand(commandType CommandType) bool {
	return isDeferredPaxdMaintenance(commandType) || commandType == CommandAttachmentEnsureLocal
}

func (s *ControlService) handleSessionRuntimeReset(ctx context.Context, src Source, cmd Command) (CommandAck, error) {
	if s.sessionRuntimeReset == nil {
		return failedAck(cmd.CommandID, "session", cmd.ResetSessionRuntime.NativeSessionID, ControlError{
			Code: ErrCodeInternal, Message: "session runtime reset is not configured",
		}), nil
	}
	result, err := s.sessionRuntimeReset.ResetSessionRuntime(ctx, src.RemoteID, *cmd.ResetSessionRuntime)
	if err != nil {
		return failedAck(cmd.CommandID, "session", cmd.ResetSessionRuntime.NativeSessionID, errorToControlError(err)), nil
	}
	return CommandAck{
		CommandID: cmd.CommandID, OK: true, Status: CommandStatusApplied,
		TargetType: "session", TargetID: cmd.ResetSessionRuntime.NativeSessionID,
		Result: &CommandResult{SessionRuntimeReset: &result},
	}, nil
}

func secretChannelIdentity(src Source) secretchannel.Identity {
	return secretchannel.Identity{Principal: string(src.Kind) + ":" + strings.TrimSpace(src.RemoteID)}
}

func (s *ControlService) handleSecretChannelOpen(src Source) (QueryResult, error) {
	if s.secretChannel == nil {
		return QueryResult{Type: QuerySecretChannelOpen, Error: ptr(ControlError{
			Code: ErrCodeInternal, Message: "secret channel is not configured",
		})}, nil
	}
	info, err := s.secretChannel.Open(secretChannelIdentity(src))
	if err != nil {
		code := ErrCodeInternal
		if errors.Is(err, secretchannel.ErrRateLimited) || errors.Is(err, secretchannel.ErrTooManyChannels) {
			code = ErrCodeConflict
		}
		return QueryResult{Type: QuerySecretChannelOpen, Error: ptr(ControlError{Code: code, Message: err.Error()})}, nil
	}
	return QueryResult{Type: QuerySecretChannelOpen, SecretChannelOpen: &SecretChannelOpenResult{
		ChannelID: info.ChannelID,
		NodeID:    info.NodeID,
		PublicKey: base64.StdEncoding.EncodeToString(info.PublicKey),
		ExpiresAt: info.ExpiresAt.UTC().Format(time.RFC3339),
	}}, nil
}

// handleSecretChannelPush deliberately bypasses Store/CommandRecord
// entirely, the same way handleSessionRuntimeReset does: the entire
// "apply" is synchronous in-memory work (decrypt + write a local file), so
// there is nothing to converge later, and nothing here needs the generic
// command audit trail. secretChannel.Consume already provides its own
// command_id-scoped idempotency (see internal/secretchannel), so this
// handler does not need to duplicate it.
func (s *ControlService) handleSecretChannelPush(_ context.Context, src Source, cmd Command) (CommandAck, error) {
	push := cmd.PushSecretChannel
	if s.secretChannel == nil {
		return failedAck(cmd.CommandID, "secret_channel", push.ChannelID, ControlError{
			Code: ErrCodeInternal, Message: "secret channel is not configured",
		}), nil
	}
	senderPub, err := base64.StdEncoding.DecodeString(push.SenderPublicKey)
	if err != nil {
		return rejectedAck(cmd.CommandID, "secret_channel", push.ChannelID,
			invalid("push_secret_channel.sender_public_key", "sender public key must be base64")), nil
	}
	nonce, err := base64.StdEncoding.DecodeString(push.Nonce)
	if err != nil {
		return rejectedAck(cmd.CommandID, "secret_channel", push.ChannelID,
			invalid("push_secret_channel.nonce", "nonce must be base64")), nil
	}
	ciphertext, err := base64.StdEncoding.DecodeString(push.Ciphertext)
	if err != nil {
		return rejectedAck(cmd.CommandID, "secret_channel", push.ChannelID,
			invalid("push_secret_channel.ciphertext", "ciphertext must be base64")), nil
	}

	result, err := s.secretChannel.Consume(secretChannelIdentity(src), secretchannel.PushRequest{
		ChannelID:       push.ChannelID,
		CommandID:       cmd.CommandID,
		SenderPublicKey: senderPub,
		Nonce:           nonce,
		Ciphertext:      ciphertext,
	})
	if err != nil {
		return failedAck(cmd.CommandID, "secret_channel", push.ChannelID, errorToControlError(err)), nil
	}

	switch result.Status {
	case secretchannel.StatusApplied:
		return CommandAck{
			CommandID: cmd.CommandID, OK: true, Status: CommandStatusApplied,
			TargetType: "secret_channel", TargetID: push.ChannelID,
			Result: &CommandResult{SecretChannelPush: &SecretChannelPushResult{
				FileRef:   result.FileRef,
				ExpiresAt: result.ExpiresAt.UTC().Format(time.RFC3339),
			}},
		}, nil
	case secretchannel.StatusConflict:
		return rejectedAck(cmd.CommandID, "secret_channel", push.ChannelID, ControlError{
			Code: ErrCodeConflict, Message: "command id was already used with a different payload",
		}), nil
	case secretchannel.StatusUnauthorized:
		return rejectedAck(cmd.CommandID, "secret_channel", push.ChannelID, ControlError{
			Code: "unauthorized", Message: "this channel was not opened by the requesting source",
		}), nil
	case secretchannel.StatusInvalidPayload:
		return rejectedAck(cmd.CommandID, "secret_channel", push.ChannelID, ControlError{
			Code: ErrCodeInvalidArgument, Message: "unable to decrypt payload",
		}), nil
	case secretchannel.StatusWriteFailed:
		return failedAck(cmd.CommandID, "secret_channel", push.ChannelID, ControlError{
			Code: ErrCodeInternal, Message: "failed to persist secret",
		}), nil
	case secretchannel.StatusConsumed:
		// Deliberately a different code from "expired": this channel_id was
		// already claimed (just not by this exact command_id), so opening a
		// new channel and delivering again would risk a real duplicate.
		// Callers must not treat this the way they treat "expired".
		return rejectedAck(cmd.CommandID, "secret_channel", push.ChannelID, ControlError{
			Code: "consumed", Message: "secret channel was already used",
		}), nil
	default: // secretchannel.StatusExpired, and any status this build doesn't know about yet.
		return rejectedAck(cmd.CommandID, "secret_channel", push.ChannelID, ControlError{
			Code: "expired", Message: "secret channel expired or unknown",
		}), nil
	}
}

func isDesiredSlotsOnlyUpdate(cmd Command) bool {
	update := cmd.UpdateAgentConnection
	return cmd.Type == CommandAgentConnectionUpdate && update != nil && update.DesiredSlots != nil &&
		update.Name == nil && update.CloudAgentID == nil && update.InstanceID == nil &&
		update.AgentType == nil && update.Harness == nil && update.Command == nil &&
		update.WorkingDir == nil && update.Env == nil && update.Enabled == nil && update.DesiredState == nil
}

func (s *ControlService) HandleQuery(ctx context.Context, src Source, query Query) (QueryResult, error) {
	if err := query.Validate(); err != nil {
		return QueryResult{Type: query.Type, Error: ptr(controlErr(err))}, nil
	}
	if query.Type == QueryHarnessAuthStatus {
		return s.handleHarnessAuthStatus(ctx, src, *query.HarnessAuthStatus)
	}
	if query.Type == QueryBrowserControl {
		if s.browserControl == nil || src.Kind != SourceLocal && (src.Kind != SourceRemote || src.RemoteID == "") {
			return QueryResult{Type: query.Type, Error: ptr(ControlError{Code: ErrCodeInternal, Message: "browser control unavailable"})}, nil
		}
		body, err := s.browserControl.Request(ctx, string(src.Kind)+":"+src.RemoteID, query.BrowserControl.Operation, query.BrowserControl.Payload)
		if err != nil {
			return QueryResult{Type: query.Type, Error: ptr(ControlError{Code: ErrCodeInternal, Message: err.Error()})}, nil
		}
		return QueryResult{Type: query.Type, BrowserControl: body}, nil
	}
	if query.Type == QuerySecretChannelOpen {
		return s.handleSecretChannelOpen(src)
	}
	if s.store == nil {
		return QueryResult{Type: query.Type, Error: ptr(ControlError{Code: ErrCodeInternal, Message: "control store is not configured"})}, nil
	}

	switch query.Type {
	case QueryStatusGet:
		return s.handleStatusQuery(ctx)
	case QueryDiagnosticsGet:
		return s.handleDiagnosticsQuery(ctx)
	case QueryRemotesList:
		items, err := s.store.ListRemotes(ctx, *query.ListRemotes)
		return QueryResult{Type: query.Type, Remotes: &ListRemotesResult{Items: items}, Error: errorPtr(err)}, nil
	case QueryRemoteGet:
		return s.handleGetRemote(ctx, *query.GetRemote)
	case QueryAgentConnectionsList:
		items, err := s.store.ListAgentConnections(ctx, *query.ListAgentConnections)
		return QueryResult{Type: query.Type, AgentConnections: &ListAgentConnectionsResult{Items: items}, Error: errorPtr(err)}, nil
	case QueryAgentConnectionGet:
		return s.handleGetAgentConnection(ctx, *query.GetAgentConnection)
	case QueryHarnessesList:
		return s.handleListHarnesses(ctx, *query.ListHarnesses)
	case QueryHarnessesDiscover:
		return s.handleDiscoverHarnesses(ctx, *query.DiscoverHarnesses)
	case QueryLocalOverviewGet:
		return s.handleLocalOverview(ctx)
	case QueryLocalSessionsList:
		return s.handleListLocalSessions(ctx, *query.ListLocalSessions)
	case QueryLocalSessionsSync:
		return s.handleSyncLocalSessions(ctx, *query.SyncLocalSessions)
	case QueryLocalSessionGet:
		return s.handleGetLocalSession(ctx, *query.GetLocalSession)
	case QueryCommandGet:
		cmd, err := s.store.GetCommand(ctx, query.GetCommand.CommandID)
		return QueryResult{Type: query.Type, Command: cmd, Error: errorPtr(err)}, nil
	case QueryAttachmentLocalStatus:
		if s.attachments == nil {
			return QueryResult{Type: query.Type, Error: ptr(ControlError{Code: ErrCodeInternal, Message: "attachment localizer is not configured"})}, nil
		}
		items, err := s.attachments.Status(ctx, src, query.GetAttachmentLocalStatus.AttachmentIDs)
		return QueryResult{Type: query.Type, AttachmentLocalStatus: &AttachmentLocalStatusResult{Items: items}, Error: errorPtr(err)}, nil
	default:
		return QueryResult{Type: query.Type, Error: ptr(ControlError{Code: ErrCodeInternal, Message: "unsupported query type"})}, nil
	}
}

func (s *ControlService) applyCommand(ctx context.Context, tx TxStore, src Source, cmd Command) (CommandAck, bool, bool, error) {
	switch cmd.Type {
	case CommandRemoteCreate:
		view, err := tx.CreateRemote(ctx, *cmd.CreateRemote)
		return receivedRemoteAck(cmd.CommandID, view), true, false, err
	case CommandRemoteUpdate:
		view, err := tx.UpdateRemote(ctx, *cmd.UpdateRemote)
		return receivedRemoteAck(cmd.CommandID, view), true, false, err
	case CommandRemoteDelete:
		view, err := tx.DeleteRemote(ctx, *cmd.DeleteRemote)
		return receivedRemoteAck(cmd.CommandID, view), true, true, err
	case CommandRemoteRestart:
		view, err := tx.RestartRemote(ctx, *cmd.RestartRemote)
		return receivedRemoteAck(cmd.CommandID, view), true, false, err
	case CommandRemoteAuthConfigure:
		err := tx.ConfigureRemoteAuth(ctx, *cmd.ConfigureRemoteAuth)
		return receivedAck(cmd.CommandID, "remote", cmd.ConfigureRemoteAuth.RemoteID), true, true, err
	case CommandRemoteAuthClear:
		err := tx.ClearRemoteAuth(ctx, *cmd.ClearRemoteAuth)
		return receivedAck(cmd.CommandID, "remote", cmd.ClearRemoteAuth.RemoteID), true, true, err
	case CommandAgentConnectionCreate:
		view, err := tx.CreateAgentConnection(ctx, *cmd.CreateAgentConnection)
		return receivedAgentConnectionAck(cmd.CommandID, view), false, true, err
	case CommandAgentConnectionUpdate:
		view, err := tx.UpdateAgentConnection(ctx, *cmd.UpdateAgentConnection)
		return receivedAgentConnectionAck(cmd.CommandID, view), false, true, err
	case CommandAgentConnectionDelete:
		view, err := tx.DeleteAgentConnection(ctx, *cmd.DeleteAgentConnection)
		return receivedAgentConnectionAck(cmd.CommandID, view), false, true, err
	case CommandAgentConnectionRestart:
		view, err := tx.RestartAgentConnection(ctx, *cmd.RestartAgentConnection)
		return receivedAgentConnectionAck(cmd.CommandID, view), false, true, err
	case CommandRestartPaxd:
		return receivedAck(cmd.CommandID, "paxd", ""), false, false, nil
	case CommandUpgradePaxd:
		return receivedAck(cmd.CommandID, "paxd", ""), false, false, nil
	case CommandCancelPaxdMaintenance:
		return receivedAck(cmd.CommandID, "paxd", cmd.CancelPaxdMaintenance.MaintenanceCommandID), false, false, nil
	case CommandAttachmentEnsureLocal:
		if s.attachments == nil {
			return CommandAck{}, false, false, errors.New("attachment localizer is not configured")
		}
		if err := s.attachments.Ensure(ctx, src, *cmd.EnsureAttachmentLocal); err != nil {
			return CommandAck{}, false, false, err
		}
		return receivedAck(cmd.CommandID, "attachment", cmd.EnsureAttachmentLocal.Attachment.AttachmentID), false, false, nil
	default:
		return CommandAck{}, false, false, fmt.Errorf("unsupported command type %q", cmd.Type)
	}
}

func (s *ControlService) handleStatusQuery(ctx context.Context) (QueryResult, error) {
	remotes, err := s.store.ListRemotes(ctx, ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		return QueryResult{Type: QueryStatusGet, Error: ptr(errorToControlError(err))}, nil
	}
	conns, err := s.store.ListAgentConnections(ctx, ListAgentConnectionsQuery{IncludeDisabled: true})
	if err != nil {
		return QueryResult{Type: QueryStatusGet, Error: ptr(errorToControlError(err))}, nil
	}
	status := DaemonStatus{Phase: "running"}
	for _, remote := range remotes {
		if remote.Status != nil {
			status.Remotes = append(status.Remotes, *remote.Status)
		}
	}
	for _, conn := range conns {
		if conn.Status != nil {
			status.AgentConnections = append(status.AgentConnections, *conn.Status)
		}
	}
	return QueryResult{Type: QueryStatusGet, Status: &status}, nil
}

func (s *ControlService) handleDiagnosticsQuery(ctx context.Context) (QueryResult, error) {
	view := DiagnosticsView{}
	if s.diagnostics != nil {
		view.RuntimeDiagnostics = s.diagnostics.RuntimeDiagnostics(ctx)
	}
	remotes, err := s.store.ListRemotes(ctx, ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		return QueryResult{Type: QueryDiagnosticsGet, Error: ptr(errorToControlError(err))}, nil
	}
	for _, remote := range remotes {
		if remote.Status != nil {
			view.Remotes = append(view.Remotes, *remote.Status)
		}
	}
	conns, err := s.store.ListAgentConnections(ctx, ListAgentConnectionsQuery{IncludeDisabled: true})
	if err != nil {
		return QueryResult{Type: QueryDiagnosticsGet, Error: ptr(errorToControlError(err))}, nil
	}
	for _, conn := range conns {
		if conn.Status != nil {
			view.AgentConnections = append(view.AgentConnections, *conn.Status)
		}
	}
	if slots, ok := s.store.(ACPSlotStatusSource); ok {
		items, err := slots.ListACPSlotStatuses(ctx)
		if err != nil {
			return QueryResult{Type: QueryDiagnosticsGet, Error: ptr(errorToControlError(err))}, nil
		}
		view.ACPSlots = items
	}
	return QueryResult{Type: QueryDiagnosticsGet, Diagnostics: &view}, nil
}

func (s *ControlService) BuildRuntimeSnapshot(ctx context.Context, remoteID string, nodeID string) (RuntimeSnapshotReport, error) {
	if s.store == nil {
		return RuntimeSnapshotReport{}, ControlError{Code: ErrCodeInternal, Message: "control store is not configured"}
	}
	conns, err := s.store.ListAgentConnections(ctx, ListAgentConnectionsQuery{RemoteID: remoteID, IncludeDisabled: true})
	if err != nil {
		return RuntimeSnapshotReport{}, err
	}
	snapshot := RuntimeSnapshotReport{
		SnapshotID: fmt.Sprintf("snap_%d", time.Now().UTC().UnixNano()),
		Agents:     make([]AgentRuntimeReport, 0, len(conns)),
	}
	if s.hostMetrics != nil {
		if host, err := s.hostMetrics.CurrentHostMetrics(ctx); err == nil && host != nil {
			copy := *host
			snapshot.Host = &copy
		}
	}
	for _, conn := range conns {
		report := AgentRuntimeReport{
			ConnectionID: conn.ID,
			CloudAgentID: conn.CloudAgentID,
			RemoteID:     conn.RemoteID,
			NodeID:       nodeID,
			Name:         conn.Name,
			AgentType:    conn.AgentType,
			DesiredState: conn.DesiredState,
		}
		if conn.Status != nil {
			report.RuntimePhase = conn.Status.Phase
			report.ObservedGeneration = conn.Status.ObservedGeneration
			report.ObservedRestartNonce = conn.Status.ObservedRestartNonce
			report.StatusUpdatedAt = conn.Status.UpdatedAt
			report.FailureClass = conn.Status.FailureClass
			report.LastErrorCode = conn.Status.LastErrorCode
			report.LastErrorMessage = conn.Status.LastErrorMessage
		}
		if s.acpPoolCapabilities != nil {
			if capabilityReport, ok := s.acpPoolCapabilities.ACPPoolCapabilityReport(ctx, conn.ID); ok && capabilityReport != nil {
				reportCopy := *capabilityReport
				report.ACPPoolCapabilityReport = &reportCopy
			}
		}
		snapshot.Agents = append(snapshot.Agents, report)
	}
	return snapshot, nil
}

func (s *ControlService) handleGetRemote(ctx context.Context, query GetRemoteQuery) (QueryResult, error) {
	items, err := s.store.ListRemotes(ctx, ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		return QueryResult{Type: QueryRemoteGet, Error: ptr(errorToControlError(err))}, nil
	}
	for _, item := range items {
		if item.Remote.ID == query.RemoteID {
			return QueryResult{Type: QueryRemoteGet, Remote: &item}, nil
		}
	}
	return QueryResult{Type: QueryRemoteGet, Error: ptr(notFoundError("remote not found", "remote_id"))}, nil
}

func (s *ControlService) handleGetAgentConnection(ctx context.Context, query GetAgentConnectionQuery) (QueryResult, error) {
	items, err := s.store.ListAgentConnections(ctx, ListAgentConnectionsQuery{IncludeDisabled: true})
	if err != nil {
		return QueryResult{Type: QueryAgentConnectionGet, Error: ptr(errorToControlError(err))}, nil
	}
	for _, item := range items {
		if item.ID == query.ConnectionID {
			return QueryResult{Type: QueryAgentConnectionGet, AgentConnection: &item}, nil
		}
	}
	return QueryResult{Type: QueryAgentConnectionGet, Error: ptr(notFoundError("agent connection not found", "connection_id"))}, nil
}

func (s *ControlService) handleListHarnesses(ctx context.Context, query ListHarnessesQuery) (QueryResult, error) {
	if s.harnesses == nil {
		return QueryResult{Type: QueryHarnessesList, Error: ptr(ControlError{Code: ErrCodeInternal, Message: "harness registry is not configured"})}, nil
	}
	items, err := s.harnesses.ListCached(ctx)
	if err == nil && !query.IncludeMissing {
		items = filterAvailableHarnesses(items)
	}
	return QueryResult{Type: QueryHarnessesList, Harnesses: &ListHarnessesResult{Items: items}, Error: errorPtr(err)}, nil
}

func filterAvailableHarnesses(items []HarnessView) []HarnessView {
	out := make([]HarnessView, 0, len(items))
	for _, item := range items {
		if item.State == "" || item.State == "available" {
			out = append(out, item)
		}
	}
	return out
}

func (s *ControlService) handleDiscoverHarnesses(ctx context.Context, query DiscoverHarnessesQuery) (QueryResult, error) {
	if s.harnesses == nil {
		return QueryResult{Type: QueryHarnessesDiscover, Error: ptr(ControlError{Code: ErrCodeInternal, Message: "harness registry is not configured"})}, nil
	}
	items, err := s.harnesses.Discover(ctx, query)
	return QueryResult{Type: QueryHarnessesDiscover, Harnesses: &ListHarnessesResult{Items: items}, Error: errorPtr(err)}, nil
}

func (s *ControlService) handleLocalOverview(ctx context.Context) (QueryResult, error) {
	if s.harnesses == nil || s.localSessions == nil {
		return QueryResult{Type: QueryLocalOverviewGet, Error: ptr(ControlError{Code: ErrCodeInternal, Message: "local observation ports are not configured"})}, nil
	}
	harnesses, err := s.harnesses.ListCached(ctx)
	if err != nil {
		return QueryResult{Type: QueryLocalOverviewGet, Error: ptr(errorToControlError(err))}, nil
	}
	sessions, err := s.localSessions.List(ctx, ListLocalSessionsQuery{})
	if err != nil {
		return QueryResult{Type: QueryLocalOverviewGet, Error: ptr(errorToControlError(err))}, nil
	}
	return QueryResult{Type: QueryLocalOverviewGet, LocalOverview: &LocalOverview{Harnesses: harnesses, Sessions: sessions}}, nil
}

func (s *ControlService) handleListLocalSessions(ctx context.Context, query ListLocalSessionsQuery) (QueryResult, error) {
	if s.localSessions == nil {
		return QueryResult{Type: QueryLocalSessionsList, Error: ptr(ControlError{Code: ErrCodeInternal, Message: "local sessions port is not configured"})}, nil
	}
	items, err := s.localSessions.List(ctx, query)
	return QueryResult{Type: QueryLocalSessionsList, LocalSessions: &ListLocalSessionsResult{Items: items}, Error: errorPtr(err)}, nil
}

func (s *ControlService) handleSyncLocalSessions(ctx context.Context, query SyncLocalSessionsQuery) (QueryResult, error) {
	if s.localSessions == nil {
		return QueryResult{Type: QueryLocalSessionsSync, Error: ptr(ControlError{Code: ErrCodeInternal, Message: "local sessions port is not configured"})}, nil
	}
	result, err := s.localSessions.Sync(ctx, query)
	return QueryResult{Type: QueryLocalSessionsSync, LocalSessionSync: &result, Error: errorPtr(err)}, nil
}

func (s *ControlService) handleGetLocalSession(ctx context.Context, query GetLocalSessionQuery) (QueryResult, error) {
	if s.localSessions == nil {
		return QueryResult{Type: QueryLocalSessionGet, Error: ptr(ControlError{Code: ErrCodeInternal, Message: "local sessions port is not configured"})}, nil
	}
	session, err := s.localSessions.Get(ctx, query)
	return QueryResult{Type: QueryLocalSessionGet, LocalSession: session, Error: errorPtr(err)}, nil
}

func commandPayloadJSON(cmd Command) (string, error) {
	raw, err := json.Marshal(cmd)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func commandTarget(cmd Command) (string, string) {
	switch cmd.Type {
	case CommandRemoteCreate:
		return "remote", cmd.CreateRemote.Remote.ID
	case CommandRemoteUpdate:
		return "remote", cmd.UpdateRemote.RemoteID
	case CommandRemoteDelete:
		return "remote", cmd.DeleteRemote.RemoteID
	case CommandRemoteRestart:
		return "remote", cmd.RestartRemote.RemoteID
	case CommandRemoteAuthConfigure:
		return "remote", cmd.ConfigureRemoteAuth.RemoteID
	case CommandRemoteAuthClear:
		return "remote", cmd.ClearRemoteAuth.RemoteID
	case CommandAgentConnectionCreate:
		return "agent_connection", cmd.CreateAgentConnection.ID
	case CommandAgentConnectionUpdate:
		return "agent_connection", cmd.UpdateAgentConnection.ConnectionID
	case CommandAgentConnectionDelete:
		return "agent_connection", cmd.DeleteAgentConnection.ConnectionID
	case CommandAgentConnectionRestart:
		return "agent_connection", cmd.RestartAgentConnection.ConnectionID
	case CommandRestartPaxd, CommandUpgradePaxd:
		return "paxd", ""
	case CommandCancelPaxdMaintenance:
		return "paxd", cmd.CancelPaxdMaintenance.MaintenanceCommandID
	case CommandAttachmentEnsureLocal:
		return "attachment", cmd.EnsureAttachmentLocal.Attachment.AttachmentID
	case CommandSessionRuntimeReset:
		return "session", cmd.ResetSessionRuntime.NativeSessionID
	case CommandSecretChannelPush:
		return "secret_channel", cmd.PushSecretChannel.ChannelID
	default:
		return "", ""
	}
}

func (s *ControlService) prepareRestartAck(ctx context.Context, cmd Command, rec CommandRecord, ack CommandAck) CommandAck {
	if rec.Status != CommandStatusReceived {
		return decorateRestartAck(ack, rec.ResultJSON)
	}
	result := PaxdRestartResult{RequestedBootID: s.paxdLifecycle.BootID()}
	if rec.ResultJSON != "" && rec.ResultJSON != "{}" {
		_ = json.Unmarshal([]byte(rec.ResultJSON), &result)
	}
	if result.RequestedBootID != "" && result.RequestedBootID != s.paxdLifecycle.BootID() {
		if err := s.store.CompleteCommand(ctx, cmd.CommandID, CommandCompletion{
			Status: CommandStatusApplied, ResultJSON: rec.ResultJSON,
		}); err != nil {
			return failedAck(cmd.CommandID, "paxd", "", errorToControlError(err))
		}
		ack.Status = CommandStatusApplied
		return decorateRestartAck(ack, rec.ResultJSON)
	}
	if rec.ResultJSON == "" || rec.ResultJSON == "{}" {
		raw, err := json.Marshal(result)
		if err != nil {
			return failedAck(cmd.CommandID, "paxd", "", ControlError{Code: ErrCodeInternal, Message: "failed to encode paxd restart result"})
		}
		rec.ResultJSON = string(raw)
		if err := s.store.CompleteCommand(ctx, cmd.CommandID, CommandCompletion{
			Status: CommandStatusReceived, ResultJSON: rec.ResultJSON,
		}); err != nil {
			return failedAck(cmd.CommandID, "paxd", "", errorToControlError(err))
		}
	}
	s.paxdLifecycle.ScheduleRestart(cmd.CommandID, *cmd.RestartPaxd)
	return decorateRestartAck(ack, rec.ResultJSON)
}

func decorateRestartAck(ack CommandAck, resultJSON string) CommandAck {
	var result PaxdRestartResult
	if json.Unmarshal([]byte(resultJSON), &result) == nil && result.RequestedBootID != "" {
		ack.Result = &CommandResult{PaxdRestart: &result}
	}
	return ack
}

func (s *ControlService) ConfirmCommandAckDelivered(commandID string) {
	if s == nil || s.store == nil || s.paxdLifecycle == nil || strings.TrimSpace(commandID) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rec, err := s.store.GetCommandRecord(ctx, commandID)
	if err != nil || !isDeferredPaxdMaintenance(rec.Type) || rec.Status != CommandStatusReceived {
		return
	}
	s.paxdLifecycle.ConfirmAckDelivered(commandID)
}

func receivedRemoteAck(commandID string, view RemoteView) CommandAck {
	ack := receivedAck(commandID, "remote", view.Remote.ID)
	ack.DesiredGeneration = view.Generation
	ack.Result = &CommandResult{Remote: &view}
	return ack
}

func receivedAgentConnectionAck(commandID string, view AgentConnectionView) CommandAck {
	ack := receivedAck(commandID, "agent_connection", view.ID)
	ack.DesiredGeneration = view.Generation
	ack.Result = &CommandResult{AgentConnection: &view}
	return ack
}

func receivedAck(commandID, targetType, targetID string) CommandAck {
	return CommandAck{CommandID: commandID, OK: true, Status: CommandStatusReceived, TargetType: targetType, TargetID: targetID}
}

func rejectedAck(commandID, targetType, targetID string, err ControlError) CommandAck {
	return CommandAck{CommandID: commandID, OK: false, Status: CommandStatusRejected, TargetType: targetType, TargetID: targetID, Error: &err}
}

func failedAck(commandID, targetType, targetID string, err ControlError) CommandAck {
	return CommandAck{CommandID: commandID, OK: false, Status: CommandStatusFailed, TargetType: targetType, TargetID: targetID, Error: &err}
}

func ackFromRecord(rec CommandRecord) CommandAck {
	ack := CommandAck{
		CommandID:         rec.CommandID,
		OK:                rec.Status == CommandStatusReceived || rec.Status == CommandStatusApplied,
		Status:            rec.Status,
		TargetType:        rec.TargetType,
		TargetID:          rec.TargetID,
		DesiredGeneration: int64Value(rec.DesiredGeneration),
	}
	if rec.ErrorCode != "" || rec.ErrorMessage != "" {
		ack.Error = &ControlError{Code: rec.ErrorCode, Message: rec.ErrorMessage}
	}
	return ack
}

func controlErr(err error) ControlError {
	if typed, ok := err.(ControlError); ok {
		return typed
	}
	return ControlError{Code: ErrCodeInternal, Message: err.Error()}
}

func errorToControlError(err error) ControlError {
	if err == nil {
		return ControlError{}
	}
	if typed, ok := err.(ControlError); ok {
		return typed
	}
	if errors.Is(err, ErrNotFound) {
		return notFoundError("not found", "")
	}
	if errors.Is(err, ErrDuplicate) {
		return ControlError{Code: ErrCodeConflict, Message: "duplicate record"}
	}
	return ControlError{Code: ErrCodeInternal, Message: err.Error()}
}

func notFoundError(message, target string) ControlError {
	return ControlError{Code: ErrCodeNotFound, Message: message, Target: target}
}

func errorPtr(err error) *ControlError {
	if err == nil {
		return nil
	}
	return ptr(errorToControlError(err))
}

func int64Value(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func ptr[T any](value T) *T {
	return &value
}
