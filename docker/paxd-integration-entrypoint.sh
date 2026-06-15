#!/bin/sh
set -eu

if [ "${PAXD_START_MOCK_HERMES:-true}" != "false" ]; then
  /app/mockhermes &
fi

exec /app/paxd run
