# User-initiated local file fetch behavior

`artifactfetch` owns the built-in rules that decide whether a web console user
may pull a file off the node, plus the content scanner that runs before any
byte leaves the machine. The control dispatcher and the artifact publisher use
these rules; the agent `publish_artifact` path never does.

Rules are built in. There is no configuration file, CLI flag, or web override.

## Module boundaries

Upstream callers:

- `internal/control` dispatches `artifact_fetch.check` and `artifact.fetch`
  from authenticated remote node-control sources to an `ArtifactFetchService`.
- `internal/artifactpublisher.FetchService` implements that port: it binds the
  source remote to the agent, persists a `user_fetch` job and wakes the worker.
- `internal/artifactpublisher.Worker` re-opens the file through the same rules
  when it snapshots it, and runs the content scanner before `prepare`.

What to mock:

- Nothing in `artifactfetch`; tests use real temporary home directories,
  real files, real permissions and real symlinks.

## Path rules (pure, table-driven)

### Scenario: an ordinary file inside home passes the path rules

Given home is `/home/me`
When the path rules evaluate `/home/me/proj/notes.md`
Then no denial is returned
And the cleaned path is `/home/me/proj/notes.md` with filename `notes.md`.

### Scenario: malformed paths are denied first

Given a path that is relative, empty, longer than 8192 bytes, or contains
NUL, CR or LF
When the path rules evaluate it
Then the denial is `invalid_path` from rule `builtin.home-root`.

### Scenario: paths outside home are denied

Given a path that leaves home after `filepath.Clean` (for example
`/home/me/../other/x` or `/etc/passwd`)
When the path rules evaluate it
Then the denial is `outside_home` from rule `builtin.home-root`.

### Scenario: hidden segments are denied unless allowlisted

Given any segment relative to home starts with `.`
When that segment is not `.git`, `.claude`, `.codex`, `.agent` or `.agents`
(case-insensitive)
Then the denial is `hidden_path_denied` from rule `builtin.hidden-path`.

The allowlist exempts only that segment. `.claude/notes.md` passes, but
`.claude/.credentials.json` and `proj/.env` are still denied.

### Scenario: known credential files are denied at any depth

Given a path ending (case-insensitively) in `.git/config`, `.codex/auth.json`,
`.codex/config.toml`, `.claude/settings*.json` or `.claude/.credentials.json`
When the path rules evaluate it
Then the denial is `credential_path_denied` from rule `builtin.credential-path`.

### Scenario: sensitive file types are denied by basename

Given a basename with extension `.pem`, `.key`, `.p12`, `.pfx`, `.keystore`,
`.jks`, `.kdbx`, `.ppk`, `.sqlite`, `.sqlite3` or `.db`, a SQLite sidecar such
as `auth.sqlite-wal` or `app.db-shm`, or a name starting with `id_rsa`,
`id_dsa`, `id_ecdsa` or `id_ed25519`
When the path rules evaluate it
Then the denial is `credential_path_denied` from rule `builtin.sensitive-file`.

### Scenario: rules are evaluated in a fixed order

Given a path that violates several rules
When it is evaluated
Then the first rule in the order invalid_path, outside_home,
hidden_path_denied, credential-path, sensitive-file wins.

## Open and stat rules

### Scenario: the file is opened through a home-rooted handle

Given a symlink inside home whose target is outside home
When the file is opened for check or fetch
Then `os.Root` refuses the traversal
And the denial is `outside_home` from rule `builtin.home-root`.

### Scenario: symlinks are judged by the file they resolve to

Given a relative symlink inside home such as `proj/notes.md -> ../.env`,
`proj/git.md -> .git/config` or a chain of such links
When the file is checked or opened
Then the path rules run again on the fully resolved path (with home resolved
too, since it may sit under a symlinked ancestor)
And the denial is the one the resolved target would get
(`hidden_path_denied`, `credential_path_denied`, ...)
And the opened handle must be the same file as the resolved path
(`os.SameFile`), otherwise a swapped link is denied as `not_found`.
A link to an ordinary file inside home stays fetchable.

### Scenario: file state problems are denials, not errors

Given the path does not exist, is a directory or device, or cannot be opened
because of EACCES
When the file is checked
Then the denial is `not_found`, `not_regular_file` or `permission_denied`
respectively, from rule `builtin.home-root`.

### Scenario: owner-only files are denied with a fix hint

Given a non-Windows node and a regular file whose opened handle reports a mode
with no group or other permission bits (for example 0600, 0400 or 0700)
When the file is checked
Then the denial is `owner_only_denied` from rule `builtin.owner-only`
And the message tells the user that running `chmod 644 <file>` locally makes
it fetchable.

The decision uses `fstat` on the opened handle, never a separate `stat` on the
path, so a swap between check and open cannot bypass it.

### Scenario: oversized files are denied

Given a regular file larger than 5 GiB
When the file is checked
Then the denial is `too_large` from rule `builtin.size-limit`.

### Scenario: a fetchable file reports its size without reading content

Given a regular 0644 file inside home that passes every rule
When `artifact_fetch.check` runs
Then the result is `allowed=true` with the cleaned path, filename and
`size_bytes`
And no file content is read.

## Content scan

### Scenario: known secret formats are detected

Given a text file containing a private key header, an AWS access key ID, a
GitHub token, a Slack token, an Anthropic or OpenAI API key, a Google API key,
a Stripe live secret key, or a pax node/user API key (`pax_` / `paxu_` plus 48
lowercase hex characters)
When the scanner reads it
Then it reports the rule name and the 1-based line of the first hit
And never returns the matched text.

### Scenario: ordinary documents pass

Given a markdown file that only mentions secrets in prose
When the scanner reads it
Then no finding is reported.

### Scenario: binary files are skipped

Given a file with a NUL byte in its first 8 KiB
When the scanner reads it
Then no finding is reported, whatever follows.

### Scenario: a secret split across a read chunk is still found

Given a scanner whose read window ends in the middle of a token
When the scanner streams the file
Then the overlapping window still reports the token with its correct line
And memory use stays bounded by the window, not the file size.
