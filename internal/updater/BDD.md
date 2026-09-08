# Paxd updater behavior

## Resolver selection

### Scenario: remote manager requests an upgrade

Given an authenticated node-control command arrives from a configured remote

And no explicit updater resolver override is configured

When paxd stages the requested version

Then it resolves the release from that remote's `cloud_api_url` at
`/api/v1/public/paxd/download`

And it does not contact the legacy hosted Pax resolver.

### Scenario: hosted tunnel is protected by Cloudflare Access

Given the remote uses `https://wsapi.lakeward.net` and no resolver override

When paxd derives its upgrade resolver

Then it uses `https://api.lakeward.net/api/v1/public/paxd/download`

And it does not change the remote's tunnel URL or follow Access login redirects.

Custom origins, ports, and path prefixes retain their existing resolver behavior.
The same hosted-origin selection applies to `paxl daemon install/update`.

An already-running older daemon needs a one-time CLI update and service restart
to load this fix; publishing the fixed binary alone cannot repair its resolver.

### Scenario: operator configures a resolver override

Given `PAXD_UPDATE_RESOLVER_URL` is set

When paxd stages an upgrade from any remote

Then it uses the explicit resolver URL without resolving the remote URL.

### Scenario: source remote cannot be resolved

Given no explicit resolver override is configured

And the authenticated source remote is missing

When paxd attempts to stage the upgrade

Then staging fails before any artifact request or executable change.

## Integrity

### Scenario: resolved artifact is valid

Given the resolver version matches the explicitly requested semantic version

When paxd downloads the artifact

Then it requires the exact advertised size and SHA-256

And executes the staged binary with `--version` before activation.

## Redirect and credential boundary

### Scenario: resolver or signed binary URL redirects

Given the selected manager resolver or its returned signed object URL responds
with HTTP 3xx

When paxd checks or stages an update

Then it does not contact the redirect target

And it reports the original 3xx status instead of accepting a later 200

And transport errors omit the complete request URL and signed query from both
runtime output and durable maintenance command state.

The direct `paxd update check --format json` response reports version, digest,
size, platform, and availability metadata but never serializes the resolver's
signed download URL.
