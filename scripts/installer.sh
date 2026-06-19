#!/usr/bin/env bash
set -euo pipefail

PAX_DOWNLOAD_URL="${PAX_DOWNLOAD_URL:-https://api.paxtech.net}"
PAX_CLOUD_URL="${PAX_CLOUD_URL:-https://api.paxtech.net}"
PAX_TAG="${PAX_TAG:-stable}"
PAX_BINARY_NAME="${PAX_BINARY_NAME:-}"
PAX_INSTALL_DIR="${PAX_INSTALL_DIR:-}"
PAX_CONNECT_AFTER_INSTALL="${PAX_CONNECT_AFTER_INSTALL:-1}"
PAX_RUN_AFTER_CONNECT="${PAX_RUN_AFTER_CONNECT:-0}"
pax_installer_tmpdir=""

if [[ -t 1 ]] && command -v tput >/dev/null 2>&1 && [[ "$(tput colors 2>/dev/null || echo 0)" -ge 8 ]]; then
  bold="$(tput bold)"
  reset="$(tput sgr0)"
  red="$(tput setaf 1)"
  green="$(tput setaf 2)"
  yellow="$(tput setaf 3)"
  magenta="$(tput setaf 5)"
  cyan="$(tput setaf 6)"
else
  bold=""
  reset=""
  red=""
  green=""
  yellow=""
  magenta=""
  cyan=""
fi

log() {
  printf '%s\n' "${cyan}==>${reset} $*"
}

warn() {
  printf '%s\n' "${yellow}warning:${reset} $*" >&2
}

fail() {
  printf '%s\n' "${red}error:${reset} $*" >&2
  exit 1
}

print_banner() {
  local width=52
  banner_line() {
    local color="$1"
    local text="$2"
    printf '%b|%b %b%-*s%b %b|%b\n' \
      "${cyan}${bold}" "${reset}" "$color" $((width - 4)) "$text" "${reset}" "${cyan}${bold}" "${reset}"
  }

  printf '%b\n' "${cyan}${bold}+--------------------------------------------------+${reset}"
  banner_line "${magenta}${bold}" "    ____  ___   _  __"
  banner_line "${magenta}${bold}" "   / __ \\/   | | |/ /"
  banner_line "${magenta}${bold}" "  / /_/ / /| | |   /"
  banner_line "${yellow}${bold}" " / ____/ ___ |/   |"
  banner_line "${yellow}${bold}" "/_/   /_/  |_/_/|_|"
  banner_line "${green}${bold}" "                 installer for your agent fleet"
  printf '%b\n' "${cyan}${bold}+--------------------------------------------------+${reset}"
  printf '\n'
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

detect_platform() {
  local os arch
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m | tr '[:upper:]' '[:lower:]')"

  case "$os" in
    darwin) os="darwin" ;;
    linux) os="linux" ;;
    msys*|mingw*|cygwin*) os="windows" ;;
    *) fail "unsupported operating system: $(uname -s)" ;;
  esac

  case "$arch" in
    arm64|aarch64) arch="arm64" ;;
    x86_64|amd64) arch="amd64" ;;
    *) fail "unsupported architecture: $(uname -m)" ;;
  esac

  printf '%s/%s' "$os" "$arch"
}

urlencode() {
  python3 - "$1" <<'PY'
import sys
import urllib.parse

print(urllib.parse.quote(sys.argv[1], safe=""))
PY
}

json_field() {
  python3 -c 'import json, sys
path = sys.argv[1].split(".")
doc = json.load(sys.stdin)
value = doc
for part in path:
    value = value[part]
print(value)' "$1"
}

path_has_dir() {
  [[ ":${PATH:-}:" == *":$1:"* ]]
}

choose_install_dir() {
  if [[ -n "$PAX_INSTALL_DIR" ]]; then
    printf '%s' "$PAX_INSTALL_DIR"
    return
  fi

  if path_has_dir /usr/local/bin; then
    printf '%s' /usr/local/bin
    return
  fi

  local dir
  IFS=':' read -r -a path_dirs <<< "${PATH:-}"
  for dir in "${path_dirs[@]}"; do
    if [[ -n "$dir" && -d "$dir" && -w "$dir" ]]; then
      printf '%s' "$dir"
      return
    fi
  done

  printf '%s' "$HOME/.local/bin"
}

download_with_progress() {
  local url="$1"
  local output="$2"

  if curl --help all 2>/dev/null | grep -q -- '--progress-bar'; then
    curl -fL --progress-bar -o "$output" "$url"
  else
    curl -fL -o "$output" "$url"
  fi
}

checksum_file() {
  local path="$1"

  if command -v shasum >/dev/null 2>&1; then
    LC_ALL=C LANG=C shasum -a 256 "$path" | awk '{print $1}'
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$path" | awk '{print $1}'
  else
    fail "missing shasum or sha256sum for checksum verification"
  fi
}

main() {
  print_banner
  require_cmd curl
  require_cmd python3

  local platform binary_name encoded_platform api response tmpdir binary_path
  local download_url sha256 size version install_dir target got_sha
  platform="$(detect_platform)"
  binary_name="$PAX_BINARY_NAME"
  if [[ -z "$binary_name" ]]; then
    binary_name="paxd"
    if [[ "$platform" == windows/* ]]; then
      binary_name="paxd.exe"
    fi
  fi

  encoded_platform="$(urlencode "$platform")"
  api="${PAX_DOWNLOAD_URL%/}/api/v1/public/paxd/download?platform=${encoded_platform}&tags=${PAX_TAG}"

  log "Detected platform: ${bold}${platform}${reset}"
  log "Resolving latest ${bold}${PAX_TAG}${reset} paxd artifact"
  response="$(curl -fsSL "$api")" || fail "failed to resolve paxd artifact from $api"
  if [[ "$response" != \{* ]]; then
    fail "expected JSON from $api; got a non-JSON response. Check whether the public paxd download endpoint is behind an auth/login redirect."
  fi

  download_url="$(printf '%s' "$response" | json_field data.url)"
  sha256="$(printf '%s' "$response" | json_field data.sha256)"
  size="$(printf '%s' "$response" | json_field data.size_bytes)"
  version="$(printf '%s' "$response" | json_field data.version)"

  tmpdir="$(mktemp -d)"
  pax_installer_tmpdir="$tmpdir"
  trap 'rm -rf "${pax_installer_tmpdir:-}"' EXIT
  binary_path="$tmpdir/$binary_name"

  log "Downloading paxd ${bold}${version}${reset} (${size} bytes)"
  download_with_progress "$download_url" "$binary_path"

  got_sha="$(checksum_file "$binary_path")"
  [[ "$got_sha" == "$sha256" ]] || fail "sha256 mismatch: got $got_sha expected $sha256"
  chmod 0755 "$binary_path"

  install_dir="$(choose_install_dir)"
  mkdir -p "$install_dir"
  target="$install_dir/$binary_name"
  log "Installing paxd to ${bold}${target}${reset}"
  if ! cp "$binary_path" "$target" 2>/dev/null; then
    if command -v sudo >/dev/null 2>&1; then
      sudo cp "$binary_path" "$target"
      sudo chmod 0755 "$target"
    else
      fail "cannot write to $install_dir and sudo is unavailable"
    fi
  fi
  chmod 0755 "$target" 2>/dev/null || true

  if ! path_has_dir "$install_dir"; then
    warn "$install_dir is not currently in PATH"
    warn "add it to your shell profile, or run paxd via: $target"
  fi

  log "Installed: $("${target}" --version)"

  if [[ "$PAX_CONNECT_AFTER_INSTALL" == "1" ]]; then
    log "Starting interactive Pax pairing"
    connect_args=(connect)
    if [[ -n "$PAX_CLOUD_URL" ]]; then
      connect_args+=(--cloud-url "${PAX_CLOUD_URL%/}")
    fi
    if [[ "$PAX_RUN_AFTER_CONNECT" == "1" ]]; then
      connect_args+=(--run)
    fi
    exec "$target" "${connect_args[@]}"
  fi

  if [[ -n "$PAX_CLOUD_URL" ]]; then
    printf '%s\n' "${green}Done.${reset} Run: ${bold}paxd connect --cloud-url ${PAX_CLOUD_URL%/}${reset}"
  else
    printf '%s\n' "${green}Done.${reset} Run: ${bold}paxd connect${reset}"
  fi
}

main "$@"
