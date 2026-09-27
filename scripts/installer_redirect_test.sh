#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=installer.sh
source "${script_dir}/installer.sh"

fail_test() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

stdin_marker="stdin-main-ran"
if ! stdin_output="$(
  awk '
    index($0, "if [[") == 1 && index($0, "BASH_SOURCE[0]") > 0 {
      print "main() { printf \"%s\\n\" \"stdin-main-ran\"; }"
    }
    { print }
  ' "${script_dir}/installer.sh" | bash 2>&1
)"; then
  fail_test "installer failed when executed from standard input: ${stdin_output}"
fi
if [[ "$stdin_output" != "$stdin_marker" ]]; then
  fail_test "installer did not run main when executed from standard input"
fi
printf 'ok - installer runs main when executed from standard input\n'

test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT
curl_call_file="${test_dir}/curl-args"
curl_mode="download"
signed_secret="installer-signed-secret"

curl() {
  if [[ "${1:-}" == "--help" ]]; then
    printf '%s\n' '--progress-bar'
    return
  fi
  printf '%s\n' "$@" >"$curl_call_file"
  if [[ "$curl_mode" == "fail" ]]; then
    printf 'curl failed for https://objects.test/paxd?X-Amz-Signature=%s\n' \
      "$signed_secret" >&2
    return 22
  fi
  local previous="" arg output=""
  for arg in "$@"; do
    if [[ "$previous" == "-o" ]]; then
      output="$arg"
      break
    fi
    previous="$arg"
  done
  [[ -n "$output" ]] || return 2
  printf 'paxd' >"$output"
}

assert_zero_redirects() {
  local args_file="$1"
  awk '
    previous == "--max-redirs" && $0 == "0" { found_limit = 1 }
    $0 ~ /^-[A-Za-z]*L[A-Za-z]*$/ { found_follow = 1 }
    { previous = $0 }
    END { exit(found_limit && found_follow ? 0 : 1) }
  ' "$args_file" || fail_test "curl was not constrained to zero redirects"
}

download_path="${test_dir}/paxd"
download_with_progress \
  "https://objects.test/paxd?X-Amz-Signature=${signed_secret}" \
  "$download_path" >/dev/null
assert_zero_redirects "$curl_call_file"
[[ "$(sed -n '1p' "$download_path")" == "paxd" ]] ||
  fail_test "binary download did not write the artifact"
printf 'ok - signed object curl follows zero redirects\n'

curl_mode="fail"
download_error="${test_dir}/download-error"
if (download_with_progress \
  "https://objects.test/paxd?X-Amz-Signature=${signed_secret}" \
  "$download_path") >"$download_error" 2>&1; then
  fail_test "binary transport failure unexpectedly succeeded"
fi
if grep -q "$signed_secret" "$download_error"; then
  fail_test "binary transport error leaked a signed URL"
fi
printf 'ok - binary transport errors redact signed URLs\n'

default_home="${test_dir}/home"
default_install_dir="${default_home}/.local/bin"
actual_install_dir="$(
  HOME="$default_home" \
    PAX_INSTALL_DIR="" \
    PATH="/usr/local/bin:/usr/bin" \
    choose_install_dir
)"
[[ "$actual_install_dir" == "$default_install_dir" ]] ||
  fail_test "default install directory was not HOME/.local/bin"
printf 'ok - default install directory is HOME/.local/bin\n'

custom_install_dir="${test_dir}/custom bin"
actual_install_dir="$(
  HOME="$default_home" \
    PAX_INSTALL_DIR="$custom_install_dir" \
    PATH="/usr/local/bin:/usr/bin" \
    choose_install_dir
)"
[[ "$actual_install_dir" == "$custom_install_dir" ]] ||
  fail_test "PAX_INSTALL_DIR did not override the default"
printf 'ok - explicit install directory overrides the default\n'

zsh_hint=""
if ! zsh_hint="$(
  HOME="$default_home" \
    SHELL="/bin/zsh" \
    PATH="/usr/bin" \
    print_path_hint "$default_install_dir" "$default_install_dir/paxd" 2>&1
)"; then
  fail_test "missing PATH guidance caused installer failure"
fi
grep -Fq 'export PATH="$HOME/.local/bin:$PATH"' <<<"$zsh_hint" ||
  fail_test "zsh PATH guidance was not copyable"
grep -Fq '"$HOME/.zshrc"' <<<"$zsh_hint" ||
  fail_test "zsh PATH guidance did not name .zshrc"
printf 'ok - zsh receives copyable PATH guidance\n'

bash_hint="$(
  HOME="$default_home" \
    SHELL="/bin/bash" \
    PATH="/usr/bin" \
    print_path_hint "$default_install_dir" "$default_install_dir/paxd" 2>&1
)" || fail_test "bash PATH guidance caused installer failure"
grep -Fq 'export PATH="$HOME/.local/bin:$PATH"' <<<"$bash_hint" ||
  fail_test "bash PATH guidance was not copyable"
grep -Fq '"$HOME/.bashrc"' <<<"$bash_hint" ||
  fail_test "bash PATH guidance did not name .bashrc"
printf 'ok - bash receives copyable PATH guidance\n'

fish_hint="$(
  HOME="$default_home" \
    SHELL="/usr/bin/fish" \
    PATH="/usr/bin" \
    print_path_hint "$default_install_dir" "$default_install_dir/paxd" 2>&1
)" || fail_test "fish PATH guidance caused installer failure"
grep -Fq 'fish_add_path "$HOME/.local/bin"' <<<"$fish_hint" ||
  fail_test "fish PATH guidance was not shell-appropriate"
printf 'ok - fish receives copyable PATH guidance\n'

generic_hint="$(
  unset SHELL
  HOME="$default_home" \
    PATH="/usr/bin" \
    print_path_hint "$default_install_dir" "$default_install_dir/paxd" 2>&1
)" || fail_test "PATH guidance required SHELL to be set"
grep -Fq 'export PATH="$HOME/.local/bin:$PATH"' <<<"$generic_hint" ||
  fail_test "generic PATH guidance was not copyable"
grep -Fq '"$HOME/.profile"' <<<"$generic_hint" ||
  fail_test "generic PATH guidance did not name .profile"
printf 'ok - unset SHELL receives portable PATH guidance\n'

path_hint="$(
  HOME="$default_home" \
    SHELL="/bin/zsh" \
    PATH="${default_install_dir}:/usr/bin" \
    print_path_hint "$default_install_dir" "$default_install_dir/paxd" 2>&1
)" || fail_test "PATH check failed when install directory was present"
[[ -z "$path_hint" ]] || fail_test "PATH guidance was printed for a directory already in PATH"
printf 'ok - no PATH guidance is printed when install directory is already present\n'

print_banner() { :; }
require_cmd() { :; }
detect_platform() { printf 'linux/amd64'; }
urlencode() { printf 'linux%%2Famd64'; }
download_and_install_product() {
  local binary_name="$2"
  local install_dir="$4"

  installed_target="${install_dir}/${binary_name}"
  printf '%s\n' '#!/bin/sh' "printf '%s\\n' 'paxd test'" >"$installed_target"
  chmod 0755 "$installed_target"
}

main_output="$(
  HOME="$default_home" \
    SHELL="/bin/zsh" \
    PATH="/usr/bin:/bin" \
    PAX_INSTALL_DIR="" \
    PAX_SETUP_AFTER_INSTALL="0" \
    main 2>&1
)" || fail_test "installer failed when HOME/.local/bin was absent from PATH"
[[ -x "$default_install_dir/paxd" ]] ||
  fail_test "installer did not write paxd to HOME/.local/bin"
grep -Fq 'installation succeeded' <<<"$main_output" ||
  fail_test "successful install without PATH did not explain its status"
grep -Fq 'export PATH="$HOME/.local/bin:$PATH"' <<<"$main_output" ||
  fail_test "successful install without PATH did not print guidance"
printf 'ok - installation succeeds outside PATH and prints guidance\n'

# Exercise setup dispatch without installing a real binary or service.
download_and_install_product() {
  installed_target="$test_dir/fake-paxd"
  cat >"$installed_target" <<'BIN'
#!/usr/bin/env bash
if [[ "${1:-}" == "--version" ]]; then
  echo 'paxd test'
else
  printf 'setup-args:%s\n' "$*"
  [[ "${PAX_REGISTRATION_TOKEN:-}" == 'test-onetime-token' ]] && echo 'token-present' || true
fi
BIN
  chmod 0755 "$installed_target"
}
setup_output="$(PAX_SETUP_AFTER_INSTALL=1 PAX_CLOUD_URL=https://test.example PAX_REGISTRATION_TOKEN=test-onetime-token main)"
grep -Fq 'setup-args:setup --registration-token-env --cloud-url https://test.example' <<<"$setup_output" || fail_test 'token setup option was not forwarded'
grep -Fq 'token-present' <<<"$setup_output" || fail_test 'token environment was not inherited by setup'
if grep -Fq 'test-onetime-token' <<<"$setup_output"; then fail_test 'token value leaked into setup output'; fi
pair_output="$(PAX_SETUP_AFTER_INSTALL=1 PAX_CLOUD_URL=https://test.example PAX_REGISTRATION_TOKEN= main)"
grep -Fq 'setup-args:setup --cloud-url https://test.example' <<<"$pair_output" || fail_test 'pairing setup was not preserved'
printf 'ok - token setup and browser pairing dispatch remain distinct\n'
