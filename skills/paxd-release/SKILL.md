---
name: paxd-release
description: Build multi-platform paxd release binaries, upload them to GCS, publish pax-manager artifact metadata, and verify stable public download resolution.
---

# Paxd Release

Use this skill when the user asks to release paxd, build multi-platform paxd binaries, upload paxd artifacts to GCS, or publish pax-manager binary metadata.

## Workflow

Run the repo script from the `paxd` repository:

```bash
scripts/release_paxd.sh <version> stable
```

The script builds:

- `darwin/arm64`
- `darwin/amd64`
- `linux/amd64`
- `linux/arm64`
- `windows/amd64`

It uploads to:

```text
gs://pax-tech-bucket/paxd/releases/<version>/
```

Then it publishes metadata to:

```text
https://api.paxtech.net/api/v1/admin/paxd/artifacts
```

Finally, it checks the public download resolver for every platform:

```text
https://api.paxtech.net/api/v1/public/paxd/download
```

## Authentication

Use the current `gcloud` account. The script obtains a bearer token with:

```bash
gcloud auth print-identity-token
```

If a specific token is needed, set `PAX_RELEASE_TOKEN` instead. Never print the token in the final answer or PR text.

## Overrides

Use environment variables only when the release target differs from the default:

```bash
PAX_MANAGER_URL=https://api.paxtech.net \
PAX_RELEASE_BUCKET=pax-tech-bucket \
PAX_RELEASE_TAGS=stable \
scripts/release_paxd.sh 0.1.1
```

For dry runs or partial reruns:

```bash
PAX_RELEASE_SKIP_UPLOAD=1 scripts/release_paxd.sh 0.1.1 stable
PAX_RELEASE_SKIP_PUBLISH=1 scripts/release_paxd.sh 0.1.1 stable
PAX_RELEASE_SKIP_VERIFY=1 scripts/release_paxd.sh 0.1.1 stable
```

## Reporting

Report the release version, build id, platforms, GCS prefix, and whether the resolver returned the expected version for every platform. Do not include signed URLs or identity tokens.
