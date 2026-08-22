#!/usr/bin/env bash
set -euo pipefail

PAX_DOWNLOAD_URL="${PAX_DOWNLOAD_URL:-https://api.lakeward.net}"
PAX_CLOUD_URL="${PAX_CLOUD_URL:-}"
PAX_TAG="${PAX_TAG:-stable}"
PAX_BINARY_NAME="${PAX_BINARY_NAME:-}"
PAX_INSTALL_DIR="${PAX_INSTALL_DIR:-}"
PAX_SETUP_AFTER_INSTALL="${PAX_SETUP_AFTER_INSTALL:-0}"
pax_installer_tmpdir=""
installed_target=""

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

  printf '%s' "$HOME/.local/bin"
}

print_path_hint() {
  local install_dir="$1"
  local target="$2"
  local shell_name="${SHELL##*/}"
  local path_command quoted_dir profile

  if path_has_dir "$install_dir"; then
    return
  fi

  warn "installation succeeded, but $install_dir is not currently in PATH"

  if [[ "$install_dir" == "$HOME/.local/bin" ]]; then
    case "$shell_name" in
      fish)
        printf '%s\n' \
          'Copy and run this command in fish:' \
          '  fish_add_path "$HOME/.local/bin"' >&2
        ;;
      zsh)
        path_command='export PATH="$HOME/.local/bin:$PATH"'
        profile='$HOME/.zshrc'
        ;;
      bash)
        path_command='export PATH="$HOME/.local/bin:$PATH"'
        profile='$HOME/.bashrc'
        ;;
      *)
        path_command='export PATH="$HOME/.local/bin:$PATH"'
        profile='$HOME/.profile'
        ;;
    esac
  elif [[ "$shell_name" == "fish" ]]; then
    printf -v quoted_dir '%q' "$install_dir"
    printf 'Copy and run this command in fish:\n  fish_add_path -- %s\n' \
      "$quoted_dir" >&2
  else
    printf -v quoted_dir '%q' "$install_dir"
    path_command="export PATH=${quoted_dir}:\$PATH"
    case "$shell_name" in
      zsh) profile='$HOME/.zshrc' ;;
      bash) profile='$HOME/.bashrc' ;;
      *) profile='$HOME/.profile' ;;
    esac
  fi

  if [[ -n "${path_command:-}" ]]; then
    printf 'Copy and run these commands in %s:\n' "${shell_name:-your shell}" >&2
    printf '  %s\n' "$path_command" >&2
    printf '  printf '\''%%s\\n'\'' %q >> "%s"\n' "$path_command" "$profile" >&2
  fi

  printf 'Until then, run paxd directly:\n  %q\n' "$target" >&2
}

download_with_progress() {
  local url="$1"
  local output="$2"

  if curl --help all 2>/dev/null | grep -q -- '--progress-bar'; then
    if ! curl -fL --max-redirs 0 --progress-bar -o "$output" "$url" 2>/dev/null; then
      fail "failed to download artifact"
    fi
  else
    if ! curl -fL --max-redirs 0 -o "$output" "$url" 2>/dev/null; then
      fail "failed to download artifact"
    fi
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

artifact_api_for() {
  local product="$1"
  local encoded_platform="$2"

  if [[ "$product" == "paxd" ]]; then
    printf '%s' "${PAX_DOWNLOAD_URL%/}/api/v1/public/paxd/download?platform=${encoded_platform}&tags=${PAX_TAG}"
    return
  fi
  printf '%s' "${PAX_DOWNLOAD_URL%/}/api/v1/public/artifacts/download?product=${product}&platform=${encoded_platform}&tags=${PAX_TAG}"
}

download_and_install_product() {
  local product="$1"
  local binary_name="$2"
  local encoded_platform="$3"
  local install_dir="$4"
  local api response download_url sha256 size version binary_path target got_sha

  api="$(artifact_api_for "$product" "$encoded_platform")"
  log "Resolving latest ${bold}${PAX_TAG}${reset} ${product} artifact"
  response="$(curl -fsSL --max-redirs 0 "$api" 2>/dev/null)" ||
    fail "failed to resolve ${product} artifact"
  if [[ "$response" != \{* ]]; then
    fail "expected JSON from $api; got a non-JSON response. Check whether the public artifact download endpoint is behind an auth/login redirect."
  fi

  download_url="$(printf '%s' "$response" | json_field data.url)"
  sha256="$(printf '%s' "$response" | json_field data.sha256)"
  size="$(printf '%s' "$response" | json_field data.size_bytes)"
  version="$(printf '%s' "$response" | json_field data.version)"
  binary_path="$pax_installer_tmpdir/$binary_name"

  log "Downloading ${product} ${bold}${version}${reset} (${size} bytes)"
  download_with_progress "$download_url" "$binary_path"

  got_sha="$(checksum_file "$binary_path")"
  [[ "$got_sha" == "$sha256" ]] || fail "${product} sha256 mismatch: got $got_sha expected $sha256"
  chmod 0755 "$binary_path"

  target="$install_dir/$binary_name"
  log "Installing ${product} to ${bold}${target}${reset}"
  if ! cp "$binary_path" "$target" 2>/dev/null; then
    if command -v sudo >/dev/null 2>&1; then
      sudo cp "$binary_path" "$target"
      sudo chmod 0755 "$target"
    else
      fail "cannot write to $install_dir and sudo is unavailable"
    fi
  fi
  chmod 0755 "$target" 2>/dev/null || true
  installed_target="$target"
}

main() {
  print_banner
  require_cmd curl
  require_cmd python3

  local platform binary_name encoded_platform tmpdir install_dir target
  platform="$(detect_platform)"
  binary_name="$PAX_BINARY_NAME"
  if [[ -z "$binary_name" ]]; then
    binary_name="paxd"
    if [[ "$platform" == windows/* ]]; then
      binary_name="paxd.exe"
    fi
  fi

  encoded_platform="$(urlencode "$platform")"

  log "Detected platform: ${bold}${platform}${reset}"

  tmpdir="$(mktemp -d)"
  pax_installer_tmpdir="$tmpdir"
  trap 'rm -rf "${pax_installer_tmpdir:-}"' EXIT

  install_dir="$(choose_install_dir)"
  mkdir -p "$install_dir"

  download_and_install_product "paxd" "$binary_name" "$encoded_platform" "$install_dir"
  target="$installed_target"

  print_path_hint "$install_dir" "$target"

  log "Installed: $("${target}" --version)"

  if [[ "$PAX_SETUP_AFTER_INSTALL" == "1" ]]; then
    log "Starting interactive Pax setup"
    setup_args=(setup)
    if [[ -n "$PAX_CLOUD_URL" ]]; then
      setup_args+=(--cloud-url "${PAX_CLOUD_URL%/}")
    fi
    exec "$target" "${setup_args[@]}"
  fi

  if [[ -n "$PAX_CLOUD_URL" ]]; then
    printf '%s\n' "${green}Done.${reset} Run: ${bold}paxl setup --with-daemon --cloud-url ${PAX_CLOUD_URL%/}${reset}"
  else
    printf '%s\n' "${green}Done.${reset} Run: ${bold}paxl setup --with-daemon${reset}"
  fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
