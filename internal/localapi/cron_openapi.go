package localapi

func cronAPIEndpoints() []apiEndpoint {
	id := apiParameter{Name: "id", In: "path", Type: "string", Required: true}
	tags := []string{"cron"}
	return []apiEndpoint{
		{Method: "GET", Path: "/v1/cron-status", Summary: "Get scheduler status.", OperationID: "cronStatus", Response: "CronStatus", Tags: tags},
		{Method: "GET", Path: "/v1/cron-jobs", Summary: "List Pax scheduled tasks.", OperationID: "listCronJobs", Response: "CronJobs", Tags: tags, Parameters: []apiParameter{{Name: "agent", In: "query", Type: "string"}}},
		{Method: "POST", Path: "/v1/cron-jobs", Summary: "Create a Pax scheduled task.", OperationID: "createCronJob", RequestBody: "CreateCronJob", Response: "CronJob", Tags: tags},
		{Method: "GET", Path: "/v1/cron-jobs/{id}", Summary: "Get a scheduled task.", OperationID: "getCronJob", Parameters: []apiParameter{id}, Response: "CronJob", Tags: tags},
		{Method: "PATCH", Path: "/v1/cron-jobs/{id}", Summary: "Update fields using expected_revision for conflict detection.", OperationID: "updateCronJob", Parameters: []apiParameter{id}, RequestBody: "UpdateCronJob", Response: "CronJob", Tags: tags},
		{Method: "DELETE", Path: "/v1/cron-jobs/{id}", Summary: "Delete a task and retain run history.", OperationID: "deleteCronJob", Parameters: []apiParameter{id}, Response: "CronDeleted", Tags: tags},
		{Method: "POST", Path: "/v1/cron-jobs/{id}/run", Summary: "Queue a manual run, including for paused tasks.", OperationID: "runCronJob", Parameters: []apiParameter{id}, Response: "CronRun", Tags: tags},
		{Method: "GET", Path: "/v1/cron-runs", Summary: "List recent runs, at most 200.", OperationID: "listCronRuns", Response: "CronRuns", Tags: tags, Parameters: []apiParameter{{Name: "job_id", In: "query", Type: "string"}, {Name: "limit", In: "query", Type: "integer"}}},
	}
}

func allAPISchemas() map[string]any {
	schemas := openAPISchemas()
	str := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer"}
	boolean := map[string]any{"type": "boolean"}
	datetime := map[string]any{"type": "string", "format": "date-time"}
	nullableTime := map[string]any{"type": "string", "format": "date-time", "nullable": true}
	config := map[string]any{
		"name": str, "agent": str, "connection_id": str, "session_mode": enumSchema("new", "reuse"), "prompt": str,
		"working_dir": str, "schedule": str, "timezone": str, "enabled": boolean,
		"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400},
	}
	schemas["CreateCronJob"] = objectSchema(copyProperties(config), "name", "connection_id", "prompt", "working_dir", "schedule")
	patch := copyProperties(config)
	patch["expected_revision"] = integer
	patch["reset_session"] = boolean
	schemas["UpdateCronJob"] = objectSchema(patch, "expected_revision")
	config["bound_session_id"] = str
	config["agent_id"], config["remote_id"], config["cloud_api_url"] = str, str, str
	config["id"], config["revision"], config["active_run_id"] = str, integer, str
	config["next_run_at"], config["created_at"], config["updated_at"] = nullableTime, datetime, datetime
	schemas["CronJob"] = objectSchema(config)
	schemas["CronJobs"] = map[string]any{"type": "array", "items": schemaRef("CronJob")}
	schemas["CronRun"] = objectSchema(map[string]any{
		"id": str, "job_id": str, "trigger": enumSchema("scheduled", "manual"),
		"status":       enumSchema("queued", "running", "succeeded", "failed", "skipped", "interrupted"),
		"scheduled_at": datetime, "started_at": nullableTime, "finished_at": nullableTime,
		"output": str, "error": str,
		"session_id": str, "local_session_id": str, "native_session_id": str, "prompt": str, "working_dir": str, "connection_id": str, "session_mode": enumSchema("new", "reuse"),
	})
	schemas["CronRuns"] = map[string]any{"type": "array", "items": schemaRef("CronRun")}
	schemas["CronStatus"] = objectSchema(map[string]any{"running": boolean})
	schemas["CronDeleted"] = objectSchema(map[string]any{"deleted": boolean})
	return schemas
}

func copyProperties(source map[string]any) map[string]any {
	copy := make(map[string]any, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}
