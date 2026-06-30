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
  PAX_RELEASE_PRODUCTS Space-separated products to build. Defaults to "paxd".
  PAX_RELEASE_BUILD_ID Build id stored in metadata. Defaults to git short SHA.
  PAX_RELEASE_TOKEN    Bearer token for admin publish. Defaults to gcloud identity token.
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
  local payload response code

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
  local product="$2"
  local platform="$3"
  local tags="$4"
  local expected_version="$5"
  local expected_generation="$6"
  local expected_sha="$7"
  local encoded_platform response version generation sha

  encoded_platform="$(urlencode "$platform")"
  if [[ "$product" == "paxd" ]]; then
    response="$(curl -sS "${manager_url%/}/api/v1/public/paxd/download?platform=${encoded_platform}&tags=${tags}")"
  else
    response="$(curl -sS "${manager_url%/}/api/v1/public/artifacts/download?product=${product}&platform=${encoded_platform}&tags=${tags}")"
  fi
  version="$(printf '%s' "$response" | json_get data.version)"
  generation="$(printf '%s' "$response" | json_get data.generation)"
  sha="$(printf '%s' "$response" | json_get data.sha256)"

  [[ "$version" == "$expected_version" ]] || fail "resolver returned version ${version} for ${product}/${platform}, expected ${expected_version}"
  [[ "$generation" == "$expected_generation" ]] || fail "resolver returned generation ${generation} for ${product}/${platform}, expected ${expected_generation}"
  [[ "$sha" == "$expected_sha" ]] || fail "resolver returned sha ${sha} for ${product}/${platform}, expected ${expected_sha}"
}

verify_installer_redirect() {
  local manager_url="$1"
  local code

  code="$(curl -sS -o /dev/null -w '%{http_code}' "${manager_url%/}/api/v1/public/paxd/install.sh")"
  [[ "$code" == "302" ]] || fail "installer endpoint returned ${code}, expected 302"
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
  local products="${PAX_RELEASE_PRODUCTS:-paxd}"
  local dist_dir="${PAX_RELEASE_DIST_DIR:-dist}"
  local build_id="${PAX_RELEASE_BUILD_ID:-}"
  local go_private="${GOPRIVATE:-github.com/pax-beehive/*}"
  local go_nosumdb="${GONOSUMDB:-${go_private}}"
  local go_noproxy="${GONOPROXY:-${go_private}}"
  local tags_json installer_tags_json paxd_endpoint artifact_endpoint token

  require_cmd git
  require_cmd curl
  require_cmd python3
  if [[ "${PAX_RELEASE_SKIP_BUILD:-0}" != "1" ]]; then
    require_cmd go
  fi
  if [[ "${PAX_RELEASE_SKIP_UPLOAD:-0}" != "1" ||
    "${PAX_RELEASE_SKIP_PUBLISH:-0}" != "1" ||
    "${PAX_RELEASE_SKIP_VERIFY:-0}" != "1" ]]; then
    require_cmd gcloud
  fi

  if [[ -z "$build_id" ]]; then
    build_id="$(git rev-parse --short HEAD)"
  fi

  tags_json="$(tag_json_array "$tags")"
  installer_tags_json="$(tag_json_array "${tags},installer")"
  paxd_endpoint="${manager_url%/}/api/v1/admin/paxd/artifacts"
  artifact_endpoint="${manager_url%/}/api/v1/admin/artifacts"

  mkdir -p "$dist_dir"

  log "release version: ${version}"
  log "build id: ${build_id}"
  log "tags: ${tags}"
  log "products: ${products}"
  log "manager: ${manager_url}"
  log "bucket: gs://${bucket}/${prefix_parent}/${version}/"

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
        log "uploading ${product} ${platform} ${output} -> gs://${bucket}/${object}"
        gcloud storage cp "$output" "gs://${bucket}/${object}" >/dev/null
      else
        log "skipping upload for ${product} ${platform}"
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
        publish_endpoint="$artifact_endpoint"
        if [[ "$product" == "paxd" ]]; then
          publish_endpoint="$paxd_endpoint"
        fi
        log "publishing metadata for ${product} ${platform}"
        publish_artifact "$publish_endpoint" "$token" "$product" "$version" "$build_id" "$tags_json" \
          "$platform" "$bucket" "$object" "$generation" "$sha" "$size" "$content_type"
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
    local installer_path installer_object installer_sha installer_size installer_desc
    local installer_generation installer_content_type

    installer_path="scripts/installer.sh"
    installer_object="${prefix_parent}/${version}/install.sh"
    installer_sha="$(sha256_file "$installer_path")"
    installer_size="$(python3 -c 'import os, sys; print(os.path.getsize(sys.argv[1]))' "$installer_path")"
    installer_content_type="text/x-shellscript"

    if [[ "${PAX_RELEASE_SKIP_UPLOAD:-0}" != "1" ]]; then
      log "uploading ${installer_path} -> gs://${bucket}/${installer_object}"
      gcloud storage cp --content-type="$installer_content_type" \
        "$installer_path" "gs://${bucket}/${installer_object}" >/dev/null
    else
      log "skipping upload for installer"
    fi

    if [[ "${PAX_RELEASE_SKIP_UPLOAD:-0}" != "1" ||
      "${PAX_RELEASE_SKIP_PUBLISH:-0}" != "1" ||
      "${PAX_RELEASE_SKIP_VERIFY:-0}" != "1" ]]; then
      installer_desc="$(describe_gcs_object "$bucket" "$installer_object")"
      installer_generation="$(printf '%s' "$installer_desc" | object_attr generation)"
      installer_size="$(printf '%s' "$installer_desc" | object_attr size)"
      installer_content_type="$(printf '%s' "$installer_desc" | object_attr content_type)"
    fi

    if [[ "${PAX_RELEASE_SKIP_PUBLISH:-0}" != "1" ]]; then
      if [[ -z "${token:-}" ]]; then
        token="${PAX_RELEASE_TOKEN:-$(gcloud auth print-identity-token)}"
      fi
      log "publishing metadata for installer"
      publish_artifact "$paxd_endpoint" "$token" "paxd" "$version" "$build_id" "$installer_tags_json" \
        "script" "$bucket" "$installer_object" "$installer_generation" "$installer_sha" \
        "$installer_size" "$installer_content_type"
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

main "$@"
