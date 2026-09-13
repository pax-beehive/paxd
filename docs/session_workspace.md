# Session workspace directories

For `session/new`, paxd resolves `cwd` on the agent machine and recursively
creates missing directories before forwarding the request to the agent.
Accepted paths are absolute paths, `~`, and paths starting with `~/`.
For example, `~/projects/new-project` creates missing parent directories under
the home directory of the user running paxd. Manager needs no API changes.

New directories use mode `0700`, subject to the process umask. Existing directory
permissions are preserved. Creation runs with paxd's filesystem permissions;
the filesystem enforces ACLs, parent traversal permissions, and read-only
mounts during the operation. There is no privilege escalation or chmod retry.

Permission failures return `workspace_permission_denied`. Other creation
failures return `workspace_create_failed`, with the underlying filesystem error.
An existing file at the workspace path returns `workspace_not_directory`.
Failed preparation prevents the session request from reaching the agent.
Directories already created are retained if a later step fails.

Resuming an existing session does not create a missing workspace;
it continues to return `workspace_not_found`.
