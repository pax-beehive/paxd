---
name: paxd-release
description: Build multi-platform paxd release binaries, upload them to S3-compatible object storage, publish pax-manager artifact metadata, and verify stable public download resolution.
---

# Paxd Release

Use this skill when the user asks to release paxd, build multi-platform paxd binaries, upload paxd artifacts to AWS S3 or an S3-compatible service, or publish pax-manager binary metadata.

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

It uploads immutable objects to:

```text
s3://<PAX_RELEASE_BUCKET>/paxd/releases/<version>/
```

Then it publishes metadata to:

```text
<PAX_RELEASE_MANAGER_URL>/api/v1/admin/paxd/artifacts
```

Finally, it checks the public download resolver for every platform:

```text
<PAX_RELEASE_MANAGER_URL>/api/v1/public/paxd/download
```

## Authentication

Use the standard AWS credential chain for object storage and set
`PAX_RELEASE_TOKEN` to a pax-manager admin API key. For MinIO or another
S3-compatible service, also set `PAX_RELEASE_S3_ENDPOINT`. Never print access
keys, bearer tokens, signed URLs, or Cloudflare Access secrets.

## Overrides

Use environment variables only when the release target differs from the default:

```bash
PAX_RELEASE_MANAGER_URL=https://api.example.com \
PAX_RELEASE_BUCKET=pax-releases \
AWS_REGION=us-east-1 \
AWS_ACCESS_KEY_ID=... \
AWS_SECRET_ACCESS_KEY=... \
PAX_RELEASE_TOKEN=... \
PAX_RELEASE_TAGS=stable \
scripts/release_paxd.sh 0.1.1
```

For self-hosted S3-compatible storage:

```bash
PAX_RELEASE_S3_ENDPOINT=https://objects.example.com \
PAX_RELEASE_BUCKET=pax-releases \
PAX_RELEASE_TOKEN=... \
scripts/release_paxd.sh 0.1.1 stable
```

If pax-manager is behind Cloudflare Access, set
`PAX_CLOUD_CF_CLIENT_ID` and `PAX_CLOUD_CF_CLIENT_SECRET` together. These
headers are sent only to manager requests, never to object storage.

For dry runs or partial reruns:

```bash
PAX_RELEASE_SKIP_UPLOAD=1 scripts/release_paxd.sh 0.1.1 stable
PAX_RELEASE_SKIP_PUBLISH=1 scripts/release_paxd.sh 0.1.1 stable
PAX_RELEASE_SKIP_VERIFY=1 scripts/release_paxd.sh 0.1.1 stable
```

## Reporting

Report the release version, build id, platforms, S3 prefix, and whether the resolver returned the expected version for every platform. Do not include signed URLs, credentials, or bearer tokens.
