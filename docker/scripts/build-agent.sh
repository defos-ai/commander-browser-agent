#!/bin/sh
set -eu

export CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off
exec go build -trimpath -buildvcs=false -o /out/commander-browser-agent ./cmd
