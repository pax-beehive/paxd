#!/usr/bin/env bash
set -euo pipefail

# CPU-frequency discovery must not run before main, even for --version. Older
# go-m1cpu releases eagerly probe IOKit and crash when a voltage-state property
# is absent on newer Apple Silicon. Check the real binary's startup lifecycle;
# this catches the regression even on machines where those properties exist.
if [[ "$(uname -s)" != Darwin || "$(uname -m)" != arm64 ]]; then
  echo "SKIP: Apple Silicon startup regression requires a native arm64 Mac"
  exit 0
fi

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
startup_tmpdir="$(mktemp -d)"
trap 'rm -rf "$startup_tmpdir"' EXIT

cd "$repo_root"
GOWORK=off CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -o "$startup_tmpdir/paxd" ./cmd/paxd

for flag in --version --help; do
  if ! GODEBUG=inittrace=1 "$startup_tmpdir/paxd" "$flag" >"$startup_tmpdir/output" 2>"$startup_tmpdir/trace"; then
    cat "$startup_tmpdir/trace" >&2
    echo "FAIL: paxd $flag could not start" >&2
    exit 1
  fi
  if ! grep -q 'paxd' "$startup_tmpdir/output"; then
    echo "FAIL: paxd $flag did not produce CLI output" >&2
    exit 1
  fi
  if ! grep -q '^init ' "$startup_tmpdir/trace"; then
    echo "FAIL: startup tracing was not captured for paxd $flag" >&2
    exit 1
  fi
  if grep -q '^init github.com/shoenig/go-m1cpu ' "$startup_tmpdir/trace"; then
    echo "FAIL: paxd $flag eagerly initialized go-m1cpu before main" >&2
    exit 1
  fi
done

echo "PASS: paxd --version and --help start without eager Apple CPU probing"
