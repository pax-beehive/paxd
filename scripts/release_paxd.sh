#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  scripts/release_paxd.sh <version> [tag[,tag...]]

Build paxd for supported platforms, upload artifacts to S3-compatible object
storage, publish artifact metadata to pax-manager, and smoke-test the public
download resolver. S3 uploads are write-once. A retry accepts an existing
object only after its size, content type, and checksums match.

Defaults:
  tags: stable
  manager URL: https://api.lakeward.net
  object prefix: paxd/releases/<version>/
  platforms: darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64

Environment overrides:
  PAX_RELEASE_MANAGER_URL Manager API base URL, also baked into the installer as its default resolver. PAX_MANAGER_URL remains an alias.
  PAX_RELEASE_BUCKET   Required S3 bucket name, except for build-only runs.
  PAX_RELEASE_PREFIX   S3 object prefix parent.
  PAX_RELEASE_S3_ENDPOINT Optional S3-compatible endpoint URL (for example MinIO).
  PAX_RELEASE_TAGS     Comma-separated tags. Overrides the second argument.
  PAX_RELEASE_PLATFORMS Space-separated GOOS/GOARCH platforms.
  PAX_RELEASE_PRODUCTS Space-separated products to build. Defaults to "paxd".
  PAX_RELEASE_BUILD_ID Build id stored in metadata. Defaults to git short SHA.
  PAX_RELEASE_TOKEN    Required bearer token for admin metadata publish.
  PAX_RELEASE_ID_TOKEN Deprecated alias for PAX_RELEASE_TOKEN.
  PAX_CLOUD_CF_CLIENT_ID Optional Cloudflare Access service-token client ID for manager calls.
  PAX_CLOUD_CF_CLIENT_SECRET Matching service-token secret; both CF values must be set together.
  AWS_REGION           AWS region used by the AWS CLI. Defaults to us-east-1.
  AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN
                       Standard AWS credential environment variables.
  PAX_RELEASE_SKIP_BUILD=1
  PAX_RELEASE_SKIP_UPLOAD=1
  PAX_RELEASE_SKIP_PUBLISH=1
  PAX_RELEASE_SKIP_VERIFY=1
  PAX_RELEASE_SKIP_INSTALLER=1
  CGO_ENABLED          Global Go cgo toggle. Defaults to 1.
  PAX_RELEASE_CGO_ENABLED_<OS>_<ARCH>
                       Optional per-platform cgo override. Release builds fail
                       when cgo is disabled unless PAX_RELEASE_ALLOW_CGO_STUB=1.
  PAX_RELEASE_CC_<OS>_<ARCH>
                       Optional C compiler for cross-compiling cgo builds,
                       e.g. PAX_RELEASE_CC_LINUX_AMD64=x86_64-linux-gnu-gcc.
                       The script auto-detects common Linux/Windows cross
                       compilers, then falls back to global CC.
  GOPRIVATE            Go private module patterns. Defaults to github.com/pax-beehive/*.
  GONOSUMDB            Go checksum DB exclusions. Defaults to GOPRIVATE.
  GONOPROXY            Go proxy exclusions. Defaults to GOPRIVATE.

Examples:
  scripts/release_paxd.sh 0.1.1 stable
  AWS_REGION=us-west-2 PAX_RELEASE_BUCKET=my-releases PAX_RELEASE_TOKEN=... scripts/release_paxd.sh 0.1.1 stable
  AWS_REGION=us-east-1 PAX_RELEASE_S3_ENDPOINT=http://127.0.0.1:9000 PAX_RELEASE_BUCKET=pax-releases PAX_RELEASE_TOKEN=... scripts/release_paxd.sh 0.1.1 stable
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

validate_cloudflare_access_credentials() {
  local client_id="${PAX_CLOUD_CF_CLIENT_ID:-}"
  local client_secret="${PAX_CLOUD_CF_CLIENT_SECRET:-}"

  if [[ -n "$client_id" && -z "$client_secret" ]] ||
    [[ -z "$client_id" && -n "$client_secret" ]]; then
    fail "PAX_CLOUD_CF_CLIENT_ID and PAX_CLOUD_CF_CLIENT_SECRET must be set together"
  fi
}

manager_curl() {
  validate_cloudflare_access_credentials
  if [[ -n "${PAX_CLOUD_CF_CLIENT_ID:-}" ]]; then
    curl \
      -H "CF-Access-Client-Id: ${PAX_CLOUD_CF_CLIENT_ID}" \
      -H "CF-Access-Client-Secret: ${PAX_CLOUD_CF_CLIENT_SECRET}" \
      "$@"
    return
  fi
  curl "$@"
}

# Public installer and resolver checks deliberately omit Cloudflare service
# credentials. These routes must work for the documented anonymous install
# command; using manager_curl here would let a release pass even when an Access
# login intercepts real users.
public_curl() {
  curl --disable --no-location --max-redirs 0 "$@"
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

compiler_env_name_for() {
  local platform="$1"

  printf 'PAX_RELEASE_CC_%s' "$(printf '%s' "$platform" | tr '[:lower:]/' '[:upper:]_')"
}

cgo_env_name_for() {
  local platform="$1"

  printf 'PAX_RELEASE_CGO_ENABLED_%s' "$(printf '%s' "$platform" | tr '[:lower:]/' '[:upper:]_')"
}

cgo_enabled_for() {
  local platform="$1"
  local env value

  env="$(cgo_env_name_for "$platform")"
  if [[ -n "${!env+x}" ]]; then
    value="${!env}"
  elif [[ -n "${CGO_ENABLED+x}" ]]; then
    value="$CGO_ENABLED"
  else
    value="1"
  fi

  case "$value" in
    0 | 1) printf '%s' "$value" ;;
    *) fail "${env}/CGO_ENABLED must be 0 or 1, got: ${value}" ;;
  esac
}

detected_cross_compiler_for() {
  local platform="$1"

  case "$platform" in
    linux/amd64)
      if command -v x86_64-linux-gnu-gcc >/dev/null 2>&1; then
        printf '%s' "x86_64-linux-gnu-gcc"
      elif command -v x86_64-linux-musl-gcc >/dev/null 2>&1; then
        printf '%s' "x86_64-linux-musl-gcc"
      elif command -v x86_64-unknown-linux-gnu-gcc >/dev/null 2>&1; then
        printf '%s' "x86_64-unknown-linux-gnu-gcc"
      elif command -v x86_64-unknown-linux-musl-gcc >/dev/null 2>&1; then
        printf '%s' "x86_64-unknown-linux-musl-gcc"
      elif command -v zig >/dev/null 2>&1; then
        printf '%s' "zig cc -target x86_64-linux-gnu"
      fi
      ;;
    linux/arm64)
      if command -v aarch64-linux-gnu-gcc >/dev/null 2>&1; then
        printf '%s' "aarch64-linux-gnu-gcc"
      elif command -v aarch64-linux-musl-gcc >/dev/null 2>&1; then
        printf '%s' "aarch64-linux-musl-gcc"
      elif command -v aarch64-unknown-linux-gnu-gcc >/dev/null 2>&1; then
        printf '%s' "aarch64-unknown-linux-gnu-gcc"
      elif command -v aarch64-unknown-linux-musl-gcc >/dev/null 2>&1; then
        printf '%s' "aarch64-unknown-linux-musl-gcc"
      elif command -v zig >/dev/null 2>&1; then
        printf '%s' "zig cc -target aarch64-linux-gnu"
      fi
      ;;
    windows/amd64)
      if command -v x86_64-w64-mingw32-gcc >/dev/null 2>&1; then
        printf '%s' "x86_64-w64-mingw32-gcc"
      elif command -v zig >/dev/null 2>&1; then
        printf '%s' "zig cc -target x86_64-windows-gnu"
      fi
      ;;
  esac
}

compiler_for() {
  local platform="$1"
  local env detected

  env="$(compiler_env_name_for "$platform")"
  detected="$(detected_cross_compiler_for "$platform")"
  if [[ -n "${!env+x}" ]]; then
    printf '%s' "${!env}"
  elif [[ -n "$detected" ]]; then
    printf '%s' "$detected"
  elif [[ -n "${CC+x}" ]]; then
    printf '%s' "$CC"
  fi
}

host_compiler_can_build_platform() {
  local platform="$1"
  local host_platform="$2"
  local target_os="${platform%/*}"
  local host_os="${host_platform%/*}"

  [[ "$platform" == "$host_platform" ]] && return 0
  [[ "$target_os" == "darwin" && "$host_os" == "darwin" ]] && return 0

  return 1
}

preflight_build_platform() {
  local platform="$1"
  local cgo_enabled="$2"
  local cc_env="$3"
  local cc_value="$4"
  local host_platform="$5"

  if [[ "$cgo_enabled" == "0" && "${PAX_RELEASE_ALLOW_CGO_STUB:-0}" != "1" ]]; then
    fail "paxd uses go-sqlite3; ${platform} with CGO_ENABLED=0 would build a SQLite stub. Set ${cc_env} for cgo cross-compiles, or set PAX_RELEASE_ALLOW_CGO_STUB=1 only for local experiments."
  fi

  if [[ "$cgo_enabled" == "1" && -z "$cc_value" ]] &&
    ! host_compiler_can_build_platform "$platform" "$host_platform"; then
    fail "cgo cross-compile for ${platform} from ${host_platform} needs ${cc_env}=<target C compiler>. Install a supported cross compiler such as x86_64-linux-gnu-gcc/aarch64-linux-gnu-gcc/x86_64-w64-mingw32-gcc or zig."
  fi
}

artifact_name_for() {
  local product="$1"
  local version="$2"
  local platform="$3"
  local os="${platform%/*}"
  local arch="${platform#*/}"
  local name="${product}_${version}_${os}_${arch}"

  if [[ "$os" == "windows" ]]; then
    name="${name}.exe"
  fi

  printf '%s' "$name"
}

command_path_for_product() {
  local product="$1"

  case "$product" in
    paxd) printf '%s' "./cmd/paxd" ;;
    *) fail "unsupported release product: ${product}" ;;
  esac
}

tag_json_array() {
  python3 - "$1" <<'PY'
import json
import sys

tags = [tag.strip() for tag in sys.argv[1].split(",") if tag.strip()]
print(json.dumps(tags, separators=(",", ":")))
PY
}

tag_list_has() {
  local tags="$1"
  local needle="$2"

  python3 - "$tags" "$needle" <<'PY'
import sys

tags = {tag.strip() for tag in sys.argv[1].split(",") if tag.strip()}
raise SystemExit(0 if sys.argv[2] in tags else 1)
PY
}

aws_s3api() {
  local args=(--region "${AWS_REGION:-us-east-1}")

  if [[ -n "${PAX_RELEASE_S3_ENDPOINT:-}" ]]; then
    args+=(--endpoint-url "$PAX_RELEASE_S3_ENDPOINT")
  fi
  aws "${args[@]}" s3api "$@"
}

sha256_hex_to_base64() {
  python3 - "$1" <<'PY'
import base64
import sys

try:
    digest = bytes.fromhex(sys.argv[1])
except ValueError as exc:
    raise SystemExit(f"invalid hex sha256: {exc}")
if len(digest) != 32:
    raise SystemExit("invalid hex sha256 length")
sys.stdout.write(base64.b64encode(digest).decode("ascii"))
PY
}

file_size() {
  python3 -c 'import os, sys; print(os.path.getsize(sys.argv[1]))' "$1"
}

normalize_public_manager_url() {
  local manager_url="$1"

  python3 - "$manager_url" <<'PY'
import sys
from urllib.parse import urlsplit

raw_url = sys.argv[1]
if not raw_url or any(character.isspace() for character in raw_url):
    raise SystemExit("manager URL must be a public HTTP(S) base URL without credentials, query, or fragment")
manager_url = raw_url.rstrip("/")
try:
    parsed = urlsplit(manager_url)
    _ = parsed.port
except ValueError:
    raise SystemExit("manager URL is not a valid public HTTP(S) base URL")
if (
    parsed.scheme.lower() not in {"http", "https"}
    or not parsed.hostname
    or parsed.username is not None
    or parsed.password is not None
    or parsed.query
    or parsed.fragment
):
    raise SystemExit("manager URL must be a public HTTP(S) base URL without credentials, query, or fragment")
sys.stdout.write(manager_url)
PY
}

bake_installer_manager_url() {
  local source_path="$1"
  local output_path="$2"
  local manager_url

  manager_url="$(normalize_public_manager_url "$3")" || return 1

  python3 - "$source_path" "$output_path" "$manager_url" <<'PY'
import os
import shlex
import stat
import sys
from pathlib import Path

source = Path(sys.argv[1])
destination = Path(sys.argv[2])
manager_url = sys.argv[3]

marker = 'PAX_DOWNLOAD_URL="${PAX_DOWNLOAD_URL:-https://api.lakeward.net}"'
text = source.read_text(encoding="utf-8")
if text.count(marker) != 1:
    raise SystemExit("paxd installer download URL marker is missing or ambiguous")
replacement = "\n".join(
    (
        "pax_release_default_download_url=" + shlex.quote(manager_url),
        'PAX_DOWNLOAD_URL="${PAX_DOWNLOAD_URL:-$pax_release_default_download_url}"',
        "unset pax_release_default_download_url",
    )
)
destination.write_text(text.replace(marker, replacement), encoding="utf-8")
os.chmod(destination, stat.S_IMODE(source.stat().st_mode))
PY
}

upload_s3_object() {
  local path="$1"
  local bucket="$2"
  local object="$3"
  local content_type="$4"
  local sha="$5"
  local checksum_sha256 put_output put_status description

  checksum_sha256="$(sha256_hex_to_base64 "$sha")"
  if put_output="$(aws_s3api put-object \
    --bucket "$bucket" \
    --key "$object" \
    --body "$path" \
    --content-type "$content_type" \
    --metadata "sha256=${sha}" \
    --checksum-sha256 "$checksum_sha256" \
    --if-none-match '*' 2>&1)"; then
    return
  else
    put_status=$?
  fi

  # A successful write can still look failed when the client loses the
  # response. Never echo AWS stderr here because it may contain sensitive
  # endpoint context. Treat any failed PUT as idempotent only when HEAD proves
  # that the immutable object is exactly the artifact we intended to upload.
  put_output=""
  log "S3 PUT exited ${put_status}; verifying an existing immutable object"
  description="$(describe_s3_object "$bucket" "$object")"
  verify_s3_description \
    "$description" \
    "$bucket" \
    "$object" \
    "$(file_size "$path")" \
    "$content_type" \
    "$sha"
  log "existing S3 object matches; continuing the release"
}

describe_s3_object() {
  local bucket="$1"
  local object="$2"
  local description

  # AWS requires checksum mode to return ChecksumSHA256. Some S3-compatible
  # services do not implement that option, so retry without it and rely on the
  # independently verified sha256 metadata in that compatibility path.
  if description="$(aws_s3api head-object \
    --bucket "$bucket" \
    --key "$object" \
    --checksum-mode ENABLED \
    --output json 2>/dev/null)"; then
    printf '%s' "$description"
    return
  fi
  aws_s3api head-object --bucket "$bucket" --key "$object" --output json
}

verify_s3_description() {
  local description="$1"
  local bucket="$2"
  local object="$3"
  local expected_size="$4"
  local expected_content_type="$5"
  local expected_sha="$6"
  local actual_size actual_content_type actual_sha actual_checksum expected_checksum

  read -r actual_size actual_content_type actual_sha actual_checksum < <(printf '%s' "$description" | python3 -c '
import json
import sys

doc = json.load(sys.stdin)
metadata = doc.get("Metadata") or {}
print(
    doc.get("ContentLength", ""),
    doc.get("ContentType", ""),
    metadata.get("sha256", ""),
    doc.get("ChecksumSHA256", ""),
)
')
  [[ "$actual_size" == "$expected_size" ]] ||
    fail "S3 object size mismatch for s3://${bucket}/${object}: ${actual_size}, expected ${expected_size}"
  [[ "$actual_content_type" == "$expected_content_type" ]] ||
    fail "S3 object content type mismatch for s3://${bucket}/${object}: ${actual_content_type}, expected ${expected_content_type}"
  [[ "$actual_sha" == "$expected_sha" ]] ||
    fail "S3 object sha256 metadata mismatch for s3://${bucket}/${object}"
  if [[ -n "$actual_checksum" ]]; then
    expected_checksum="$(sha256_hex_to_base64 "$expected_sha")"
    [[ "$actual_checksum" == "$expected_checksum" ]] ||
      fail "S3 object checksum mismatch for s3://${bucket}/${object}"
  fi
}

release_token() {
  if [[ -n "${PAX_RELEASE_TOKEN:-}" ]]; then
    printf '%s' "$PAX_RELEASE_TOKEN"
    return
  fi
  if [[ -n "${PAX_RELEASE_ID_TOKEN:-}" ]]; then
    printf '%s' "$PAX_RELEASE_ID_TOKEN"
    return
  fi
  fail "PAX_RELEASE_TOKEN is required to publish artifact metadata"
}

publish_artifact() {
  local endpoint="$1"
  local token="$2"
  local product="$3"
  local version="$4"
  local build_id="$5"
  local tags_json="$6"
  local platform="$7"
  local bucket="$8"
  local object="$9"
  local generation="${10}"
  local sha="${11}"
  local size="${12}"
  local content_type="${13}"
  local payload response code resolved_generation

  payload="$(mktemp)"
  python3 - "$payload" <<PY
import json
import sys

payload = {
    "product": "$product",
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

  response="$(manager_curl -sS -X POST "$endpoint" \
    -H "Authorization: Bearer ${token}" \
    -H "Content-Type: application/json" \
    -d "@${payload}")"
  rm -f "$payload"

  code="$(printf '%s' "$response" | json_get code)"
  [[ "$code" == "200" ]] || fail "publish failed for ${platform}: ${response}"
  if ! resolved_generation="$(printf '%s' "$response" | json_get data.artifact.generation)"; then
    fail "publish response is missing the resolved generation for ${platform}"
  fi
  [[ "$resolved_generation" =~ ^[1-9][0-9]*$ ]] ||
    fail "publish returned invalid resolved generation for ${platform}"
  printf '%s' "$resolved_generation"
}

verify_resolver() {
  local manager_url="$1"
  local product="$2"
  local platform="$3"
  local tags="$4"
  local expected_version="$5"
  local expected_generation="$6"
  local expected_sha="$7"
  local encoded_platform response version generation sha

  encoded_platform="$(urlencode "$platform")"
  if [[ "$product" == "paxd" ]]; then
    response="$(public_curl -sS "${manager_url%/}/api/v1/public/paxd/download?platform=${encoded_platform}&tags=${tags}")"
  else
    response="$(public_curl -sS "${manager_url%/}/api/v1/public/artifacts/download?product=${product}&platform=${encoded_platform}&tags=${tags}")"
  fi
  version="$(printf '%s' "$response" | json_get data.version)"
  generation="$(printf '%s' "$response" | json_get data.generation)"
  sha="$(printf '%s' "$response" | json_get data.sha256)"

  [[ "$version" == "$expected_version" ]] || fail "resolver returned version ${version} for ${product}/${platform}, expected ${expected_version}"
  if [[ "$expected_generation" == "0" ]]; then
    [[ "$generation" =~ ^[1-9][0-9]*$ ]] ||
      fail "resolver returned non-positive generation ${generation} for ${product}/${platform}"
  else
    [[ "$generation" == "$expected_generation" ]] ||
      fail "resolver returned generation ${generation} for ${product}/${platform}, expected ${expected_generation}"
  fi
  [[ "$sha" == "$expected_sha" ]] || fail "resolver returned sha ${sha} for ${product}/${platform}, expected ${expected_sha}"
}

signed_urls_reference_same_object() {
  printf '%s\0%s\0' "$1" "$2" | python3 -c '
import sys
from urllib.parse import urlsplit

values = sys.stdin.buffer.read().split(b"\0")
if len(values) != 3 or values[-1] != b"":
    raise SystemExit(1)
try:
    left = urlsplit(values[0].decode("utf-8"))
    right = urlsplit(values[1].decode("utf-8"))
except (UnicodeDecodeError, ValueError):
    raise SystemExit(1)

def authority(value):
    if value.scheme not in {"http", "https"} or not value.hostname:
        raise ValueError("invalid URL")
    if value.username is not None or value.password is not None or value.fragment:
        raise ValueError("unsafe URL")
    port = value.port
    if port is None:
        port = 443 if value.scheme == "https" else 80
    return value.scheme, value.hostname.lower(), port

try:
    matches = authority(left) == authority(right) and left.path == right.path
except ValueError:
    matches = False
raise SystemExit(0 if matches else 1)
'
}

verify_installer_redirect() {
  local manager_url="$1"
  local result code location resolver_result resolver_code resolver_response expected_url

  result="$(public_curl -sS -o /dev/null -w $'%{http_code}\n%{redirect_url}' \
    "${manager_url%/}/api/v1/public/paxd/install.sh")" ||
    fail "installer endpoint verification request failed"
  code="${result%%$'\n'*}"
  location="${result#*$'\n'}"
  [[ "$code" == "302" && -n "$location" && "$location" != "$result" ]] ||
    fail "installer endpoint did not return an artifact redirect"

  resolver_result="$(public_curl -sS -w $'\n%{http_code}' \
    "${manager_url%/}/api/v1/public/paxd/download?platform=script&tags=stable%2Cinstaller")" ||
    fail "installer resolver verification request failed"
  resolver_code="${resolver_result##*$'\n'}"
  resolver_response="${resolver_result%$'\n'*}"
  [[ "$resolver_code" == "200" && "$resolver_response" != "$resolver_result" ]] ||
    fail "installer resolver did not return HTTP 200"
  expected_url="$(printf '%s' "$resolver_response" | json_get data.url 2>/dev/null)" ||
    fail "installer resolver did not return artifact metadata"
  signed_urls_reference_same_object "$location" "$expected_url" ||
    fail "installer endpoint redirect does not match the resolved installer artifact"
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
  local manager_url="${PAX_RELEASE_MANAGER_URL:-${PAX_MANAGER_URL:-https://api.lakeward.net}}"
  local bucket="${PAX_RELEASE_BUCKET:-}"
  local prefix_parent="${PAX_RELEASE_PREFIX:-paxd/releases}"
  local platforms="${PAX_RELEASE_PLATFORMS:-darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64}"
  local products="${PAX_RELEASE_PRODUCTS:-paxd}"
  local dist_dir="${PAX_RELEASE_DIST_DIR:-dist}"
  local build_id="${PAX_RELEASE_BUILD_ID:-}"
  local go_private="${GOPRIVATE:-github.com/pax-beehive/*}"
  local go_nosumdb="${GONOSUMDB:-${go_private}}"
  local go_noproxy="${GONOPROXY:-${go_private}}"
  local tags_json installer_tags_json paxd_endpoint artifact_endpoint token

  manager_url="$(normalize_public_manager_url "$manager_url")" || return 1
  validate_cloudflare_access_credentials

  require_cmd git
  require_cmd curl
  require_cmd python3
  if [[ "${PAX_RELEASE_SKIP_BUILD:-0}" != "1" ]]; then
    require_cmd go
  fi
  if [[ "${PAX_RELEASE_SKIP_UPLOAD:-0}" != "1" ||
    "${PAX_RELEASE_SKIP_PUBLISH:-0}" != "1" ||
    "${PAX_RELEASE_SKIP_VERIFY:-0}" != "1" ]]; then
    [[ -n "$bucket" ]] || fail "PAX_RELEASE_BUCKET is required for upload, publish, or verification"
    require_cmd aws
  fi

  if [[ -z "$build_id" ]]; then
    build_id="$(git rev-parse --short HEAD)"
  fi

  tags_json="$(tag_json_array "$tags")"
  installer_tags_json="$(tag_json_array "${tags},installer")"
  paxd_endpoint="${manager_url%/}/api/v1/admin/paxd/artifacts"
  artifact_endpoint="${manager_url%/}/api/v1/admin/artifacts"
  if [[ "${PAX_RELEASE_SKIP_PUBLISH:-0}" != "1" ]]; then
    token="$(release_token)"
  fi

  mkdir -p "$dist_dir"

  log "release version: ${version}"
  log "build id: ${build_id}"
  log "tags: ${tags}"
  log "products: ${products}"
  log "manager: ${manager_url}"
  if [[ -n "$bucket" ]]; then
    log "bucket: s3://${bucket}/${prefix_parent}/${version}/"
  else
    log "bucket: not configured (build-only)"
  fi
  if [[ -n "${PAX_RELEASE_S3_ENDPOINT:-}" ]]; then
    log "S3 endpoint: ${PAX_RELEASE_S3_ENDPOINT}"
  fi

  if [[ "${PAX_RELEASE_SKIP_BUILD:-0}" != "1" ]]; then
    local host_platform preflight_platform preflight_cgo preflight_cc_env preflight_cc_value

    host_platform="$(go env GOHOSTOS)/$(go env GOHOSTARCH)"
    for preflight_platform in $platforms; do
      preflight_cgo="$(cgo_enabled_for "$preflight_platform")"
      preflight_cc_env="$(compiler_env_name_for "$preflight_platform")"
      preflight_cc_value="$(compiler_for "$preflight_platform")"
      preflight_build_platform "$preflight_platform" "$preflight_cgo" \
        "$preflight_cc_env" "$preflight_cc_value" "$host_platform"
    done
  fi

  local platform product os arch name output object sha size content_type desc generation cc_env cc_value cgo_enabled cmd_path publish_endpoint
  for platform in $platforms; do
    os="${platform%/*}"
    arch="${platform#*/}"
    cc_env="$(compiler_env_name_for "$platform")"
    cc_value="$(compiler_for "$platform")"
    cgo_enabled="$(cgo_enabled_for "$platform")"

    for product in $products; do
      cmd_path="$(command_path_for_product "$product")"
      name="$(artifact_name_for "$product" "$version" "$platform")"
      output="${dist_dir}/${name}"

      if [[ "${PAX_RELEASE_SKIP_BUILD:-0}" != "1" ]]; then
        if [[ -n "$cc_value" ]]; then
          log "building ${product} ${platform} -> ${output} (cgo=${cgo_enabled}, cc=${cc_value})"
        else
          log "building ${product} ${platform} -> ${output} (cgo=${cgo_enabled})"
        fi
        if [[ -n "$cc_value" ]]; then
          GOPRIVATE="${go_private}" \
            GONOSUMDB="${go_nosumdb}" \
            GONOPROXY="${go_noproxy}" \
            GOCACHE="${GOCACHE:-/tmp/paxd-go-cache-release-${version//./-}}" \
            GOMODCACHE="${GOMODCACHE:-/tmp/paxd-go-mod-cache-release-${version//./-}}" \
            CGO_ENABLED="$cgo_enabled" GOOS="$os" GOARCH="$arch" CC="$cc_value" \
            go build -trimpath -ldflags="-s -w -X main.version=${version}" -o "$output" "$cmd_path"
        else
          GOPRIVATE="${go_private}" \
            GONOSUMDB="${go_nosumdb}" \
            GONOPROXY="${go_noproxy}" \
            GOCACHE="${GOCACHE:-/tmp/paxd-go-cache-release-${version//./-}}" \
            GOMODCACHE="${GOMODCACHE:-/tmp/paxd-go-mod-cache-release-${version//./-}}" \
            CGO_ENABLED="$cgo_enabled" GOOS="$os" GOARCH="$arch" \
            go build -trimpath -ldflags="-s -w -X main.version=${version}" -o "$output" "$cmd_path"
        fi
      else
        log "using prebuilt ${product} ${platform} artifact: ${output}"
        [[ -f "$output" ]] || fail "missing prebuilt artifact: ${output}"
      fi

      sha="$(sha256_file "$output")"
      size="$(python3 -c 'import os, sys; print(os.path.getsize(sys.argv[1]))' "$output")"
      content_type="$(content_type_for "$platform")"
      object="${prefix_parent}/${version}/${name}"

      if [[ "${PAX_RELEASE_SKIP_UPLOAD:-0}" != "1" ]]; then
        log "uploading ${product} ${platform} ${output} -> s3://${bucket}/${object}"
        upload_s3_object "$output" "$bucket" "$object" "$content_type" "$sha"
      else
        log "skipping upload for ${product} ${platform}"
      fi

      if [[ "${PAX_RELEASE_SKIP_UPLOAD:-0}" == "1" &&
        "${PAX_RELEASE_SKIP_PUBLISH:-0}" == "1" &&
        "${PAX_RELEASE_SKIP_VERIFY:-0}" == "1" ]]; then
        continue
      fi

      desc="$(describe_s3_object "$bucket" "$object")"
      verify_s3_description "$desc" "$bucket" "$object" "$size" "$content_type" "$sha"
      generation="0"

      if [[ "${PAX_RELEASE_SKIP_PUBLISH:-0}" != "1" ]]; then
        publish_endpoint="$artifact_endpoint"
        if [[ "$product" == "paxd" ]]; then
          publish_endpoint="$paxd_endpoint"
        fi
        log "publishing metadata for ${product} ${platform}"
        generation="$(publish_artifact \
          "$publish_endpoint" "$token" "$product" "$version" "$build_id" "$tags_json" \
          "$platform" "$bucket" "$object" "$generation" "$sha" "$size" "$content_type")"
      else
        log "skipping metadata publish for ${product} ${platform}"
      fi

      if [[ "${PAX_RELEASE_SKIP_VERIFY:-0}" != "1" ]]; then
        log "verifying public resolver for ${product} ${platform}"
        verify_resolver "$manager_url" "$product" "$platform" "$tags" "$version" "$generation" "$sha"
      fi
    done
  done

  if [[ "${PAX_RELEASE_SKIP_INSTALLER:-0}" != "1" ]]; then
    local installer_source installer_path installer_object installer_sha installer_size installer_desc
    local installer_generation installer_content_type

    installer_source="scripts/installer.sh"
    installer_path="${dist_dir}/paxd_${version}_install.sh"
    bake_installer_manager_url "$installer_source" "$installer_path" "$manager_url"
    installer_object="${prefix_parent}/${version}/install.sh"
    installer_sha="$(sha256_file "$installer_path")"
    installer_size="$(python3 -c 'import os, sys; print(os.path.getsize(sys.argv[1]))' "$installer_path")"
    installer_content_type="text/x-shellscript"

    if [[ "${PAX_RELEASE_SKIP_UPLOAD:-0}" != "1" ]]; then
      log "uploading ${installer_path} -> s3://${bucket}/${installer_object}"
      upload_s3_object "$installer_path" "$bucket" "$installer_object" \
        "$installer_content_type" "$installer_sha"
    else
      log "skipping upload for installer"
    fi

    if [[ "${PAX_RELEASE_SKIP_UPLOAD:-0}" != "1" ||
      "${PAX_RELEASE_SKIP_PUBLISH:-0}" != "1" ||
      "${PAX_RELEASE_SKIP_VERIFY:-0}" != "1" ]]; then
      installer_desc="$(describe_s3_object "$bucket" "$installer_object")"
      verify_s3_description "$installer_desc" "$bucket" "$installer_object" \
        "$installer_size" "$installer_content_type" "$installer_sha"
      installer_generation="0"
    fi

    if [[ "${PAX_RELEASE_SKIP_PUBLISH:-0}" != "1" ]]; then
      log "publishing metadata for installer"
      installer_generation="$(publish_artifact \
        "$paxd_endpoint" "$token" "paxd" "$version" "$build_id" "$installer_tags_json" \
        "script" "$bucket" "$installer_object" "$installer_generation" "$installer_sha" \
        "$installer_size" "$installer_content_type")"
    else
      log "skipping metadata publish for installer"
    fi

    if [[ "${PAX_RELEASE_SKIP_VERIFY:-0}" != "1" ]] && tag_list_has "$tags" "stable"; then
      log "verifying public installer redirect"
      verify_installer_redirect "$manager_url"
    fi
  else
    log "skipping installer artifact"
  fi

  log "release ${version} complete"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
