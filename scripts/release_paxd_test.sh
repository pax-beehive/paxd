#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=release_paxd.sh
source "${script_dir}/release_paxd.sh"
production_manager_curl="$(declare -f manager_curl)"
production_public_curl="$(declare -f public_curl)"

fail_test() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

assert_equal() {
  local expected="$1"
  local actual="$2"
  local name="$3"

  [[ "$actual" == "$expected" ]] ||
    fail_test "${name}: got ${actual}, expected ${expected}"
  printf 'ok - %s\n' "$name"
}

installer_test_dir="$(mktemp -d)"
trap 'rm -rf "$installer_test_dir"' EXIT
installer_test_path="${installer_test_dir}/install.sh"
bake_installer_manager_url \
  "${script_dir}/installer.sh" \
  "$installer_test_path" \
  "https://pax.home.example/base/"
installer_preamble="$(sed -n '4,6p' "$installer_test_path")"
assert_equal \
  "https://pax.home.example/base" \
  "$(env -u PAX_DOWNLOAD_URL bash -c "${installer_preamble}; printf '%s' \"\$PAX_DOWNLOAD_URL\"")" \
  "released installer defaults binary resolution to its manager"
assert_equal \
  "https://override.example" \
  "$(PAX_DOWNLOAD_URL=https://override.example bash -c "${installer_preamble}; printf '%s' \"\$PAX_DOWNLOAD_URL\"")" \
  "runtime PAX_DOWNLOAD_URL overrides the baked manager"
grep -Fq 'curl -fsSL --max-redirs 0 "$api"' "$installer_test_path" ||
  fail_test "installer resolver curl can follow a redirect"
grep -Fq 'curl -fL --max-redirs 0 --progress-bar -o "$output" "$url"' "$installer_test_path" ||
  fail_test "installer progress download can follow a signed URL redirect"
grep -Fq 'curl -fL --max-redirs 0 -o "$output" "$url"' "$installer_test_path" ||
  fail_test "installer fallback download can follow a signed URL redirect"
printf 'ok - released installer rejects resolver and signed URL redirects\n'

installer_error="${installer_test_dir}/error"
if bake_installer_manager_url \
  "${script_dir}/installer.sh" \
  "$installer_test_path" \
  "https://user:top-secret@pax.home.example" 2>"$installer_error"; then
  fail_test "installer accepted manager credentials in the public URL"
fi
if grep -q "top-secret" "$installer_error"; then
  fail_test "invalid manager URL leaked credentials"
fi
printf 'ok - installer rejects credential-bearing manager URLs without leaking them\n'

early_call_marker="${installer_test_dir}/early-network-call"
early_failure_output="${installer_test_dir}/early-failure-output"
if (
  aws() {
    : >"$early_call_marker"
  }
  curl() {
    : >"$early_call_marker"
  }
  PAX_RELEASE_MANAGER_URL="https://user:early-secret@pax.home.example"
  main "9.9.9"
) >"$early_failure_output" 2>&1; then
  fail_test "release main accepted a credential-bearing manager URL"
fi
if [[ -e "$early_call_marker" ]]; then
  fail_test "release made an AWS or manager request before validating the manager URL"
fi
if grep -q "early-secret" "$early_failure_output"; then
  fail_test "early manager URL validation leaked credentials"
fi
printf 'ok - release rejects an invalid manager URL before network calls without leaking it\n'

empty_sha_hex="e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
empty_sha_base64="47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="
head_checksum="$empty_sha_base64"
head_content_type="application/octet-stream"
head_sha="$empty_sha_hex"
head_size="123"
checksum_mode_supported="1"
put_should_fail="0"
put_error_message="simulated put failure"
put_expected_body="test-binary"
put_expected_object="paxd/releases/9.9.9/paxd"
put_expected_content_type="application/octet-stream"
put_expected_sha="$empty_sha_hex"
put_expected_checksum="$empty_sha_base64"

file_size() {
  printf '%s' "123"
}

aws() {
  [[ "$1" == "--region" && "$2" == "us-east-1" ]] || return 1
  [[ "$3" == "s3api" ]] || return 1
  case "$4" in
    put-object)
      [[ "$#" == "18" ]] || return 1
      [[ "$5" == "--bucket" && "$6" == "test-bucket" ]] || return 1
      [[ "$7" == "--key" && "$8" == "$put_expected_object" ]] || return 1
      [[ "$9" == "--body" && "${10}" == "$put_expected_body" ]] || return 1
      [[ "${11}" == "--content-type" && "${12}" == "$put_expected_content_type" ]] || return 1
      [[ "${13}" == "--metadata" && "${14}" == "sha256=${put_expected_sha}" ]] || return 1
      [[ "${15}" == "--checksum-sha256" && "${16}" == "$put_expected_checksum" ]] || return 1
      [[ "${17}" == "--if-none-match" && "${18}" == "*" ]] || return 1
      if [[ "$put_should_fail" == "1" ]]; then
        printf '%s\n' "$put_error_message" >&2
        return 9
      fi
      ;;
    head-object)
      if [[ "$#" == "12" ]]; then
        [[ "$9" == "--checksum-mode" && "${10}" == "ENABLED" ]] || return 1
        [[ "${11}" == "--output" && "${12}" == "json" ]] || return 1
        [[ "$checksum_mode_supported" == "1" ]] || return 2
      else
        [[ "$#" == "10" ]] || return 1
        [[ "$9" == "--output" && "${10}" == "json" ]] || return 1
      fi
      printf '{"ContentLength":%s,"ContentType":"%s","Metadata":{"sha256":"%s"},"ChecksumSHA256":"%s"}' \
        "$head_size" "$head_content_type" "$head_sha" "$head_checksum"
      ;;
    *)
      return 1
      ;;
  esac
}

installer_baked_sha="$(sha256_file "$installer_test_path")"
put_expected_body="$installer_test_path"
put_expected_object="paxd/releases/9.9.9/install.sh"
put_expected_content_type="text/x-shellscript"
put_expected_sha="$installer_baked_sha"
put_expected_checksum="$(sha256_hex_to_base64 "$installer_baked_sha")"
if ! upload_s3_object \
  "$installer_test_path" \
  "test-bucket" \
  "$put_expected_object" \
  "$put_expected_content_type" \
  "$installer_baked_sha"; then
  fail_test "release did not upload the manager-baked installer body"
fi
printf 'ok - release uploads the manager-baked installer body\n'
put_expected_body="test-binary"
put_expected_object="paxd/releases/9.9.9/paxd"
put_expected_content_type="application/octet-stream"
put_expected_sha="$empty_sha_hex"
put_expected_checksum="$empty_sha_base64"

if ! upload_s3_object \
  "test-binary" \
  "test-bucket" \
  "paxd/releases/9.9.9/paxd" \
  "application/octet-stream" \
  "$empty_sha_hex"; then
  fail_test "S3 upload was not guarded by If-None-Match"
fi
printf 'ok - S3 upload is write-once\n'

secret_marker="AWS_SECRET_ACCESS_KEY=must-not-appear"
retry_output=""
if ! retry_output="$(
  put_should_fail="1"
  put_error_message="$secret_marker"
  upload_s3_object \
    "test-binary" \
    "test-bucket" \
    "paxd/releases/9.9.9/paxd" \
    "application/octet-stream" \
    "$empty_sha_hex" 2>&1
)"; then
  fail_test "a failed PUT with a matching existing object was not idempotent"
fi
if [[ "$retry_output" == *"$secret_marker"* ]]; then
  fail_test "S3 PUT stderr leaked into release output"
fi
printf 'ok - failed PUT accepts a strictly matching existing object without leaking stderr\n'

if (
  put_should_fail="1"
  head_sha="different-sha256"
  upload_s3_object \
    "test-binary" \
    "test-bucket" \
    "paxd/releases/9.9.9/paxd" \
    "application/octet-stream" \
    "$empty_sha_hex"
) >/dev/null 2>&1; then
  fail_test "a failed PUT accepted a different existing object"
fi
printf 'ok - failed PUT rejects a mismatched existing object\n'

assert_equal \
  "$empty_sha_base64" \
  "$(sha256_hex_to_base64 "$empty_sha_hex")" \
  "hex sha256 converts to base64 without platform-specific tools"

description="$(describe_s3_object "test-bucket" "paxd/releases/9.9.9/paxd")"
verify_s3_description \
  "$description" \
  "test-bucket" \
  "paxd/releases/9.9.9/paxd" \
  "123" \
  "application/octet-stream" \
  "$empty_sha_hex"
printf 'ok - head verification checks the returned checksum\n'

if (
  head_checksum="wrong-checksum"
  description="$(describe_s3_object "test-bucket" "paxd/releases/9.9.9/paxd")"
  verify_s3_description \
    "$description" \
    "test-bucket" \
    "paxd/releases/9.9.9/paxd" \
    "123" \
    "application/octet-stream" \
    "$empty_sha_hex"
) >/dev/null 2>&1; then
  fail_test "head verification accepted a mismatched checksum"
fi
printf 'ok - head verification rejects a mismatched checksum\n'

if ! (
  checksum_mode_supported="0"
  head_checksum=""
  description="$(describe_s3_object "test-bucket" "paxd/releases/9.9.9/paxd")"
  verify_s3_description \
    "$description" \
    "test-bucket" \
    "paxd/releases/9.9.9/paxd" \
    "123" \
    "application/octet-stream" \
    "$empty_sha_hex"
) >/dev/null 2>&1; then
  fail_test "head verification did not fall back for a service without checksum mode"
fi
printf 'ok - head verification falls back for S3-compatible services without checksum mode\n'

publish_with_generation() {
  local response_generation="$1"

  manager_curl() {
    local data_arg="${!#}"
    local payload_path

    [[ "$data_arg" == @* ]] || return 1
    payload_path="${data_arg#@}"
    python3 - "$payload_path" <<'PY' || return
import json
import sys

with open(sys.argv[1], encoding="utf-8") as payload_file:
    payload = json.load(payload_file)
if payload["generation"] != 0:
    raise SystemExit(f"published generation = {payload['generation']}, expected 0")
PY
    printf '{"code":200,"data":{"artifact":{"generation":%s}}}' "$response_generation"
  }

  publish_artifact \
    "https://manager.example.test/api/v1/admin/paxd/artifacts" \
    "test-token" \
    "paxd" \
    "9.9.9" \
    "test-build" \
    '["stable"]' \
    "darwin/arm64" \
    "test-bucket" \
    "paxd/releases/9.9.9/paxd" \
    "0" \
    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" \
    "123" \
    "application/octet-stream"
}

resolved_generation=837462
published_generation="$(publish_with_generation "$resolved_generation")"
assert_equal \
  "$resolved_generation" \
  "$published_generation" \
  "publish returns manager-resolved generation"

if (publish_with_generation "0") >/dev/null 2>&1; then
  fail_test "publish accepted a non-positive manager-resolved generation"
fi
printf 'ok - publish rejects a non-positive manager-resolved generation\n'

public_curl() {
  printf '{"code":200,"data":{"version":"9.9.9","generation":837462,"sha256":"expected-sha"}}'
}

verify_resolver \
  "https://manager.example.test" \
  "paxd" \
  "darwin/arm64" \
  "stable" \
  "9.9.9" \
  "$published_generation" \
  "expected-sha"
printf 'ok - resolver exactly matches published generation\n'

if (
  public_curl() {
    printf '{"code":200,"data":{"version":"9.9.9","generation":837463,"sha256":"expected-sha"}}'
  }
  verify_resolver \
    "https://manager.example.test" \
    "paxd" \
    "darwin/arm64" \
    "stable" \
    "9.9.9" \
    "$published_generation" \
    "expected-sha"
) >/dev/null 2>&1; then
  fail_test "resolver accepted a generation different from the publish response"
fi
printf 'ok - resolver rejects a generation different from the publish response\n'

verify_resolver \
  "https://manager.example.test" \
  "paxd" \
  "darwin/arm64" \
  "stable" \
  "9.9.9" \
  "0" \
  "expected-sha"
printf 'ok - resolver accepts a positive generation when no publish response is available\n'

if (
  public_curl() {
    printf '{"code":200,"data":{"version":"9.9.9","generation":0,"sha256":"expected-sha"}}'
  }
  verify_resolver \
    "https://manager.example.test" \
    "paxd" \
    "darwin/arm64" \
    "stable" \
    "9.9.9" \
    "0" \
    "expected-sha"
) >/dev/null 2>&1; then
  fail_test "resolver accepted generation 0 after generation 0 metadata publish"
fi
printf 'ok - resolver rejects a non-positive resolved generation\n'

installer_redirect_mode="artifact"
installer_resolver_status="200"
public_curl() {
  local endpoint="${!#}"
  if [[ "$endpoint" == *"/api/v1/public/paxd/install.sh" ]]; then
    if [[ "$installer_redirect_mode" == "login" ]]; then
      printf '302\nhttps://access.example.test/cdn-cgi/access/login?state=cf-secret'
    else
      printf '302\nhttps://objects.example.test/releases/install.sh?X-Amz-Signature=first-secret'
    fi
    return
  fi
  printf '{"code":200,"data":{"url":"https://objects.example.test/releases/install.sh?X-Amz-Signature=second-secret"}}\n%s' \
    "$installer_resolver_status"
}

verify_installer_redirect "https://manager.example.test"
printf 'ok - installer redirect matches the public resolver artifact\n'

installer_redirect_mode="login"
installer_redirect_error="${installer_test_dir}/installer-redirect-error"
if (verify_installer_redirect "https://manager.example.test") >"$installer_redirect_error" 2>&1; then
  fail_test "installer verification accepted a Cloudflare login redirect"
fi
if grep -q "cf-secret" "$installer_redirect_error"; then
  fail_test "installer verification leaked the rejected redirect URL"
fi
printf 'ok - installer verification rejects an unrelated login redirect\n'

installer_redirect_mode="artifact"
installer_resolver_status="302"
if (verify_installer_redirect "https://manager.example.test") >/dev/null 2>&1; then
  fail_test "installer verification accepted a non-200 JSON resolver"
fi
printf 'ok - installer verification requires an HTTP 200 JSON resolver\n'

if (
  eval "$production_public_curl"
  PAX_CLOUD_CF_CLIENT_ID="release-client-id"
  PAX_CLOUD_CF_CLIENT_SECRET="release-client-secret"
  curl() {
    local argument endpoint="${!#}" has_access_header="0"
    for argument in "$@"; do
      if [[ "$argument" == CF-Access-Client-Id:* ||
        "$argument" == CF-Access-Client-Secret:* ]]; then
        has_access_header="1"
      fi
    done
    if [[ "$has_access_header" == "1" ]]; then
      if [[ "$endpoint" == *"/api/v1/public/paxd/install.sh" ]]; then
        printf '302\nhttps://objects.example.test/releases/install.sh?X-Amz-Signature=service-token-secret'
        return
      fi
      printf '{"code":200,"data":{"url":"https://objects.example.test/releases/install.sh?X-Amz-Signature=service-token-secret"}}\n200'
      return
    fi
    if [[ "$endpoint" == *"/api/v1/public/paxd/install.sh" ]]; then
      printf '302\nhttps://access.example.test/cdn-cgi/access/login'
      return
    fi
    printf '<html>Cloudflare Access login</html>\n302'
  }
  verify_installer_redirect "https://manager.example.test"
) >/dev/null 2>&1; then
  fail_test "public installer verification passed only because CF service-token headers were attached"
fi
printf 'ok - public installer verification is anonymous and fails behind CF Access\n'

public_curl_args="${installer_test_dir}/public-curl-args"
(
  eval "$production_public_curl"
  curl() {
    printf '%s\n' "$@" >"$public_curl_args"
  }
  public_curl -sS "https://manager.example.test/api/v1/public/paxd/download"
)
[[ "$(sed -n '1p' "$public_curl_args")" == "--disable" &&
  "$(sed -n '2p' "$public_curl_args")" == "--no-location" &&
  "$(sed -n '3p' "$public_curl_args")" == "--max-redirs" &&
  "$(sed -n '4p' "$public_curl_args")" == "0" ]] ||
  fail_test "public curl did not disable curlrc and redirects"
printf 'ok - public curl disables curlrc and follows zero redirects\n'

manager_curl_args="${installer_test_dir}/manager-curl-args"
(
  eval "$production_manager_curl"
  PAX_CLOUD_CF_CLIENT_ID="release-client-id"
  PAX_CLOUD_CF_CLIENT_SECRET="release-client-secret"
  curl() {
    printf '%s\n' "$@" >"$manager_curl_args"
  }
  manager_curl -sS "https://manager.example.test/api/v1/admin/paxd/artifacts"
)
grep -Fq 'CF-Access-Client-Id: release-client-id' "$manager_curl_args" ||
  fail_test "admin manager curl omitted the CF client ID"
grep -Fq 'CF-Access-Client-Secret: release-client-secret' "$manager_curl_args" ||
  fail_test "admin manager curl omitted the CF client secret"
printf 'ok - admin manager calls retain CF service-token headers\n'
