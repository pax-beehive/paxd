#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  scripts/release_paxd.sh <version> [tag[,tag...]]

Build paxd for supported platforms, upload artifacts to GCS, publish artifact
metadata to pax-manager, and smoke-test the public download resolver.

Defaults:
  tags: stable
  manager URL: https://api.paxtech.net
  bucket: pax-tech-bucket
  object prefix: paxd/releases/<version>/
  platforms: darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64

Environment overrides:
  PAX_MANAGER_URL      Manager API base URL.
  PAX_RELEASE_BUCKET   GCS bucket name.
  PAX_RELEASE_PREFIX   GCS object prefix parent.
  PAX_RELEASE_TAGS     Comma-separated tags. Overrides the second argument.
  PAX_RELEASE_PLATFORMS Space-separated GOOS/GOARCH platforms.
  PAX_RELEASE_BUILD_ID Build id stored in metadata. Defaults to git short SHA.
  PAX_RELEASE_TOKEN    Bearer token for admin publish. Defaults to gcloud identity token.
  PAX_RELEASE_SKIP_UPLOAD=1
  PAX_RELEASE_SKIP_PUBLISH=1
  PAX_RELEASE_SKIP_VERIFY=1

Example:
  scripts/release_paxd.sh 0.1.1 stable
EOF
}

log() {
  printf '==> %s\n' "$*"
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

json_get() {
  python3 -c 'import json, sys
path = sys.argv[1].split(".")
doc = json.load(sys.stdin)
value = doc
for part in path:
    value = value[part]
print(value)' "$1"
}

urlencode() {
  python3 - "$1" <<'PY'
import sys
import urllib.parse

print(urllib.parse.quote(sys.argv[1], safe=""))
PY
}

sha256_file() {
  local path="$1"

  if command -v shasum >/dev/null 2>&1; then
    LC_ALL=C LANG=C shasum -a 256 "$path" | awk '{print $1}'
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$path" | awk '{print $1}'
  else
    fail "missing shasum or sha256sum"
  fi
}

content_type_for() {
  local platform="$1"

  case "$platform" in
    windows/*) printf '%s' "application/x-msdownload" ;;
    *) printf '%s' "application/octet-stream" ;;
  esac
}

artifact_name_for() {
  local version="$1"
  local platform="$2"
  local os="${platform%/*}"
  local arch="${platform#*/}"
  local name="paxd_${version}_${os}_${arch}"

  if [[ "$os" == "windows" ]]; then
    name="${name}.exe"
  fi

  printf '%s' "$name"
}

tag_json_array() {
  python3 - "$1" <<'PY'
import json
import sys

tags = [tag.strip() for tag in sys.argv[1].split(",") if tag.strip()]
print(json.dumps(tags, separators=(",", ":")))
PY
}

describe_gcs_object() {
  local bucket="$1"
  local object="$2"

  gcloud storage objects describe "gs://${bucket}/${object}" --format=json
}

object_attr() {
  python3 -c 'import json
import sys

doc = json.load(sys.stdin)
key = sys.argv[1]
aliases = {
    "content_type": ["contentType", "content_type"],
    "generation": ["generation"],
    "size": ["size", "sizeBytes"],
}
for candidate in aliases.get(key, [key]):
    if candidate in doc and doc[candidate] not in (None, ""):
        print(doc[candidate])
        raise SystemExit(0)
raise SystemExit(f"missing {key} in gcloud object description")
' "$1"
}

publish_artifact() {
  local endpoint="$1"
  local token="$2"
  local version="$3"
  local build_id="$4"
  local tags_json="$5"
  local platform="$6"
  local bucket="$7"
  local object="$8"
  local generation="$9"
  local sha="${10}"
  local size="${11}"
  local content_type="${12}"
  local payload response code

  payload="$(mktemp)"
  python3 - "$payload" <<PY
import json
import sys

payload = {
    "platform": "$platform",
    "tags": json.loads('$tags_json'),
    "version": "$version",
    "build_id": "$build_id",
    "bucket": "$bucket",
    "object": "$object",
    "generation": int("$generation"),
    "sha256": "$sha",
    "size_bytes": int("$size"),
    "content_type": "$content_type",
}
with open(sys.argv[1], "w", encoding="utf-8") as f:
    json.dump(payload, f, separators=(",", ":"))
PY

  response="$(curl -sS -X POST "$endpoint" \
    -H "Authorization: Bearer ${token}" \
    -H "Content-Type: application/json" \
    -d "@${payload}")"
  rm -f "$payload"

  code="$(printf '%s' "$response" | json_get code)"
  [[ "$code" == "200" ]] || fail "publish failed for ${platform}: ${response}"
}

verify_resolver() {
  local manager_url="$1"
  local platform="$2"
  local tags="$3"
  local expected_version="$4"
  local expected_generation="$5"
  local expected_sha="$6"
  local encoded_platform response version generation sha

  encoded_platform="$(urlencode "$platform")"
  response="$(curl -sS "${manager_url%/}/api/v1/public/paxd/download?platform=${encoded_platform}&tags=${tags}")"
  version="$(printf '%s' "$response" | json_get data.version)"
  generation="$(printf '%s' "$response" | json_get data.generation)"
  sha="$(printf '%s' "$response" | json_get data.sha256)"

  [[ "$version" == "$expected_version" ]] || fail "resolver returned version ${version} for ${platform}, expected ${expected_version}"
  [[ "$generation" == "$expected_generation" ]] || fail "resolver returned generation ${generation} for ${platform}, expected ${expected_generation}"
  [[ "$sha" == "$expected_sha" ]] || fail "resolver returned sha ${sha} for ${platform}, expected ${expected_sha}"
}

main() {
  if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
    usage
    exit 0
  fi

  local version="${1:-}"
  [[ -n "$version" ]] || {
    usage
    exit 1
  }

  local tags="${PAX_RELEASE_TAGS:-${2:-stable}}"
  local manager_url="${PAX_MANAGER_URL:-https://api.paxtech.net}"
  local bucket="${PAX_RELEASE_BUCKET:-pax-tech-bucket}"
  local prefix_parent="${PAX_RELEASE_PREFIX:-paxd/releases}"
  local platforms="${PAX_RELEASE_PLATFORMS:-darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64}"
  local dist_dir="${PAX_RELEASE_DIST_DIR:-dist}"
  local build_id="${PAX_RELEASE_BUILD_ID:-}"
  local tags_json endpoint token

  require_cmd go
  require_cmd git
  require_cmd curl
  require_cmd python3
  if [[ "${PAX_RELEASE_SKIP_UPLOAD:-0}" != "1" ||
    "${PAX_RELEASE_SKIP_PUBLISH:-0}" != "1" ||
    "${PAX_RELEASE_SKIP_VERIFY:-0}" != "1" ]]; then
    require_cmd gcloud
  fi

  if [[ -z "$build_id" ]]; then
    build_id="$(git rev-parse --short HEAD)"
  fi

  tags_json="$(tag_json_array "$tags")"
  endpoint="${manager_url%/}/api/v1/admin/paxd/artifacts"

  mkdir -p "$dist_dir"

  log "release version: ${version}"
  log "build id: ${build_id}"
  log "tags: ${tags}"
  log "manager: ${manager_url}"
  log "bucket: gs://${bucket}/${prefix_parent}/${version}/"

  local platform os arch name output object sha size content_type desc generation
  for platform in $platforms; do
    os="${platform%/*}"
    arch="${platform#*/}"
    name="$(artifact_name_for "$version" "$platform")"
    output="${dist_dir}/${name}"

    log "building ${platform} -> ${output}"
    GOCACHE="${GOCACHE:-/tmp/paxd-go-cache-release-${version//./-}}" \
      GOMODCACHE="${GOMODCACHE:-/tmp/paxd-go-mod-cache-release-${version//./-}}" \
      CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
      go build -trimpath -ldflags="-s -w -X main.version=${version}" -o "$output" ./cmd/paxd

    sha="$(sha256_file "$output")"
    size="$(python3 -c 'import os, sys; print(os.path.getsize(sys.argv[1]))' "$output")"
    content_type="$(content_type_for "$platform")"
    object="${prefix_parent}/${version}/${name}"

    if [[ "${PAX_RELEASE_SKIP_UPLOAD:-0}" != "1" ]]; then
      log "uploading ${output} -> gs://${bucket}/${object}"
      gcloud storage cp "$output" "gs://${bucket}/${object}" >/dev/null
    else
      log "skipping upload for ${platform}"
    fi

    if [[ "${PAX_RELEASE_SKIP_UPLOAD:-0}" == "1" &&
      "${PAX_RELEASE_SKIP_PUBLISH:-0}" == "1" &&
      "${PAX_RELEASE_SKIP_VERIFY:-0}" == "1" ]]; then
      continue
    fi

    desc="$(describe_gcs_object "$bucket" "$object")"
    generation="$(printf '%s' "$desc" | object_attr generation)"
    size="$(printf '%s' "$desc" | object_attr size)"
    content_type="$(printf '%s' "$desc" | object_attr content_type)"

    if [[ "${PAX_RELEASE_SKIP_PUBLISH:-0}" != "1" ]]; then
      if [[ -z "${token:-}" ]]; then
        token="${PAX_RELEASE_TOKEN:-$(gcloud auth print-identity-token)}"
      fi
      log "publishing metadata for ${platform}"
      publish_artifact "$endpoint" "$token" "$version" "$build_id" "$tags_json" \
        "$platform" "$bucket" "$object" "$generation" "$sha" "$size" "$content_type"
    else
      log "skipping metadata publish for ${platform}"
    fi

    if [[ "${PAX_RELEASE_SKIP_VERIFY:-0}" != "1" ]]; then
      log "verifying public resolver for ${platform}"
      verify_resolver "$manager_url" "$platform" "$tags" "$version" "$generation" "$sha"
    fi
  done

  log "release ${version} complete"
}

main "$@"
