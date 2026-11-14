#!/bin/sh
set -eu

cd /opt/hermes
export UV_PYTHON_DOWNLOADS=never UV_LINK_MODE=copy PYTHONDONTWRITEBYTECODE=1
/tmp/uv sync --frozen --no-dev --no-install-project --no-build \
    --python /usr/local/bin/python3 --no-cache

groupadd --gid 1000 commander
useradd --uid 1000 --gid 1000 --create-home commander
install -d -m 700 -o 1000 -g 1000 /home/commander/state
