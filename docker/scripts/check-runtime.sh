#!/bin/sh
set -eu

test "$(timeout 10s "$LIGHTPANDA_PATH" version 2>/dev/null)" = "1.0.2"
exec /opt/hermes/.venv/bin/python -I -S /usr/local/lib/commander-browser/hermes/check.py
