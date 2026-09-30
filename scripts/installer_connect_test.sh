#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${script_dir}/installer.sh"

test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT

fail_test() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

print_banner() { :; }
detect_platform() { printf '%s' "${PAX_TEST_PLATFORM:-linux/amd64}"; }

# Replace only artifact delivery; main still controls installation and setup.
download_and_install_product() {
  local product="$1" binary_name="$2" install_dir="$4"
  printf 'download:%s\n' "$product" >>"$PAX_TEST_EVENTS"
  printf 'temp:%s\n' "$pax_installer_tmpdir" >>"$PAX_TEST_EVENTS"
  if [[ "${PAX_TEST_FAIL_PRODUCT:-}" == "$product" ]]; then
    return 22
  fi
  installed_target="$install_dir/$binary_name"
  cat >"$installed_target" <<'BIN'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "version" || "${1:-}" == "--version" ]]; then
  printf '%s\n' 'test version'
  exit 0
fi
[[ "${1:-}" == "setup" ]] || exit 2
printf '%s\n' 'setup' >>"$PAX_TEST_EVENTS"
printf 'arg:%s\n' "$@" >>"$PAX_TEST_EVENTS"
[[ "$(command -v paxl)" == "$PAX_INSTALL_DIR/paxl" ]] || exit 3
[[ "$(command -v paxd)" == "$PAX_INSTALL_DIR/paxd" ]] || exit 4
paxl version >/dev/null
if [[ -n "${PAX_REGISTRATION_TOKEN:-}" ]]; then
  printf '%s\n' 'token-present' >>"$PAX_TEST_EVENTS"
fi
exit "${PAX_TEST_SETUP_EXIT:-0}"
BIN
  chmod 0755 "$installed_target"
}

run_installer() (
  local scenario="$1"
  export PAX_INSTALL_DIR="$test_dir/$scenario/bin with spaces"
  export PAX_TEST_EVENTS="$test_dir/$scenario/events"
  mkdir -p "$PAX_INSTALL_DIR"
  : >"$PAX_TEST_EVENTS"
  unset SHELL
  main
)

assert_cleaned() {
  local scenario="$1" line
  while IFS= read -r line; do
    if [[ "$line" == temp:* && -e "${line#temp:}" ]]; then
      fail_test "$scenario left temporary downloads behind"
    fi
  done <"$test_dir/$scenario/events"
}

PAX_SETUP_AFTER_INSTALL=1 PAX_REGISTRATION_TOKEN=test-onetime-token \
  PAX_CLOUD_URL=https://cloud.example/ run_installer token >"$test_dir/token.log" 2>&1 ||
  fail_test 'token connection did not prepare both tools before setup'
token_events="$test_dir/token/events"
[[ -x "$test_dir/token/bin with spaces/paxl" ]] || fail_test 'paxl was not installed'
[[ -x "$test_dir/token/bin with spaces/paxd" ]] || fail_test 'paxd was not installed'
grep -Fxq 'arg:--registration-token-env' "$token_events" || fail_test 'token mode missing'
grep -Fxq 'arg:https://cloud.example' "$token_events" || fail_test 'cloud URL missing'
grep -Fxq 'token-present' "$token_events" || fail_test 'setup did not receive token'
if grep -Fq 'test-onetime-token' "$test_dir/token.log" "$token_events"; then
  fail_test 'token value was printed'
fi
assert_cleaned token
printf 'ok - token connection installs both tools and resolves them outside the original PATH\n'

PAX_SETUP_AFTER_INSTALL=1 PAX_REGISTRATION_TOKEN= PAX_CLOUD_URL= \
  run_installer pairing >"$test_dir/pairing.log" 2>&1 || fail_test 'browser connection failed'
grep -Fxq setup "$test_dir/pairing/events" || fail_test 'browser setup was not started'
if grep -Eq 'token-present|arg:--registration-token-env|arg:--cloud-url' "$test_dir/pairing/events"; then
  fail_test 'browser connection received token mode or an empty cloud override'
fi
assert_cleaned pairing
printf 'ok - browser connection installs both tools and preserves default login\n'

PAX_SETUP_AFTER_INSTALL=0 PAX_REGISTRATION_TOKEN= \
  run_installer standalone >"$test_dir/standalone.log" 2>&1 || fail_test 'standalone install failed'
[[ -x "$test_dir/standalone/bin with spaces/paxd" ]] || fail_test 'standalone paxd missing'
[[ ! -e "$test_dir/standalone/bin with spaces/paxl" ]] || fail_test 'standalone installed paxl'
if grep -Fxq setup "$test_dir/standalone/events"; then fail_test 'standalone started setup'; fi
assert_cleaned standalone
printf 'ok - standalone install remains daemon-only\n'

for product in paxl paxd; do
  if PAX_SETUP_AFTER_INSTALL=1 PAX_REGISTRATION_TOKEN=test-onetime-token \
    PAX_TEST_FAIL_PRODUCT="$product" run_installer "fail-$product" >"$test_dir/fail-$product.log" 2>&1; then
    fail_test "$product install failure was ignored"
  fi
  if grep -Fxq setup "$test_dir/fail-$product/events"; then
    fail_test "setup ran after $product installation failed"
  fi
  assert_cleaned "fail-$product"
done
printf 'ok - either installation failure stops before device registration\n'

setup_status=0
PAX_SETUP_AFTER_INSTALL=1 PAX_REGISTRATION_TOKEN= PAX_TEST_SETUP_EXIT=17 \
  run_installer failed-setup >"$test_dir/failed-setup.log" 2>&1 || setup_status=$?
[[ "$setup_status" == 17 ]] || fail_test 'setup failure status was not propagated'
assert_cleaned failed-setup
printf 'ok - setup failure is preserved without leftover downloads\n'

if PAX_SETUP_AFTER_INSTALL=1 PAX_TEST_PLATFORM=windows/amd64 \
  run_installer unsupported >"$test_dir/unsupported.log" 2>&1; then
  fail_test 'unsupported device setup was accepted'
fi
[[ ! -s "$test_dir/unsupported/events" ]] || fail_test 'unsupported setup downloaded artifacts'
printf 'ok - unsupported setup stops before downloading either tool\n'
