package control

import (
	"context"
	"encoding/json"
)

func isPaxdMaintenanceCommand(commandType CommandType) bool {
	return isDeferredPaxdMaintenance(commandType) || commandType == CommandCancelPaxdMaintenance
}

func isDeferredPaxdMaintenance(commandType CommandType) bool {
	return commandType == CommandRestartPaxd || commandType == CommandUpgradePaxd
}

func (s *ControlService) prepareMaintenanceAck(
	ctx context.Context,
	cmd Command,
	rec CommandRecord,
	ack CommandAck,
) CommandAck {
	if rec.Status != CommandStatusReceived {
		return decorateMaintenanceAck(cmd.Type, ack, rec.ResultJSON)
	}
	requestedBootID := maintenanceRequestedBootID(cmd.Type, rec.ResultJSON)
	if requestedBootID != "" && requestedBootID != s.paxdLifecycle.BootID() {
		if err := s.store.CompleteCommand(ctx, cmd.CommandID, CommandCompletion{
			Status: CommandStatusApplied, ResultJSON: rec.ResultJSON,
		}); err != nil {
			return failedAck(cmd.CommandID, "paxd", "", errorToControlError(err))
		}
		ack.Status = CommandStatusApplied
		return decorateMaintenanceAck(cmd.Type, ack, rec.ResultJSON)
	}
	if rec.ResultJSON == "" || rec.ResultJSON == "{}" {
		var result any
		if cmd.Type == CommandUpgradePaxd {
			result = PaxdUpgradeResult{
				RequestedBootID: s.paxdLifecycle.BootID(),
				TargetVersion:   cmd.UpgradePaxd.Version,
				Phase:           "awaiting_ack",
			}
		} else {
			result = PaxdRestartResult{
				RequestedBootID: s.paxdLifecycle.BootID(), Phase: "awaiting_ack",
			}
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return failedAck(cmd.CommandID, "paxd", "", ControlError{
				Code: ErrCodeInternal, Message: "failed to encode paxd maintenance result",
			})
		}
		rec.ResultJSON = string(raw)
		if err := s.store.CompleteCommand(ctx, cmd.CommandID, CommandCompletion{
			Status: CommandStatusReceived, ResultJSON: rec.ResultJSON,
		}); err != nil {
			return failedAck(cmd.CommandID, "paxd", "", errorToControlError(err))
		}
	}
	var scheduleErr error
	if cmd.Type == CommandUpgradePaxd {
		scheduleErr = s.paxdLifecycle.ScheduleUpgrade(cmd.CommandID, *cmd.UpgradePaxd)
	} else {
		scheduleErr = s.paxdLifecycle.ScheduleRestart(cmd.CommandID, *cmd.RestartPaxd)
	}
	if scheduleErr != nil {
		controlErr := errorToControlError(scheduleErr)
		_ = s.store.CompleteCommand(ctx, cmd.CommandID, CommandCompletion{
			Status: CommandStatusFailed, ResultJSON: rec.ResultJSON,
			ErrorCode: controlErr.Code, ErrorMessage: controlErr.Message,
		})
		return failedAck(cmd.CommandID, "paxd", "", controlErr)
	}
	return decorateMaintenanceAck(cmd.Type, ack, rec.ResultJSON)
}

func maintenanceRequestedBootID(commandType CommandType, resultJSON string) string {
	if commandType == CommandUpgradePaxd {
		var result PaxdUpgradeResult
		_ = json.Unmarshal([]byte(resultJSON), &result)
		return result.RequestedBootID
	}
	var result PaxdRestartResult
	_ = json.Unmarshal([]byte(resultJSON), &result)
	return result.RequestedBootID
}

func decorateMaintenanceAck(commandType CommandType, ack CommandAck, resultJSON string) CommandAck {
	if commandType == CommandUpgradePaxd {
		var result PaxdUpgradeResult
		if json.Unmarshal([]byte(resultJSON), &result) == nil && result.RequestedBootID != "" {
			ack.Result = &CommandResult{PaxdUpgrade: &result}
		}
		return ack
	}
	var result PaxdRestartResult
	if json.Unmarshal([]byte(resultJSON), &result) == nil && result.RequestedBootID != "" {
		ack.Result = &CommandResult{PaxdRestart: &result}
	}
	return ack
}
