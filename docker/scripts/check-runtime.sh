#!/bin/sh
set -eu

exec /opt/hermes/.venv/bin/python -I -S /usr/local/lib/commander-browser/hermes/check.py
