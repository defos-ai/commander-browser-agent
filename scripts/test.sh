#!/usr/bin/env bash
# Run the browser mission against its own Commander controller and API.
set -euo pipefail
cd "$(dirname "$0")/.."

if (( $# > 1 )) || [[ ${1:-} == -* ]]; then
    echo 'Usage: scripts/test.sh [results-directory]' >&2
    exit 2
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/browser-e2e-XXXXXXXX")
project=$(basename "$work" | tr '[:upper:]' '[:lower:]')
results=${1:-state/e2e/$project}
export BROWSER_TEST_BINARY="$work/browser-mission.test"

compose() {
    docker compose --env-file /dev/null -p "$project" -f tests/e2e/compose.yaml "$@"
}

builder=
built_image=
stack_started=false
cleanup() {
    local status=$?
    trap - EXIT
    set +e
    if [[ $stack_started == true ]]; then
        compose logs --no-color > "$results/compose.log"
        compose --profile test down --volumes --remove-orphans --timeout 30 || status=1
    fi
    if [[ -n $builder ]]; then docker buildx rm "$builder" || status=1; fi
    if [[ -n $built_image ]]; then docker image rm "$built_image" || status=1; fi
    rm -rf -- "$work"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "$results"
platform=linux/$(docker version --format '{{.Server.Arch}}')

# CI supplies the production image it has just built and checked.
if [[ -z ${BROWSER_TEST_AGENT_IMAGE:-} ]]; then
    export BROWSER_TEST_AGENT_IMAGE="$project:test"
    docker buildx create --name "$project" --driver docker-container \
        --driver-opt cpu-period=100000,cpu-quota=400000,memory=3500m,memory-swap=3500m
    builder=$project
    docker buildx build --builder "$builder" --platform "$platform" \
        -f docker/Dockerfile -t "$BROWSER_TEST_AGENT_IMAGE" --load .
    built_image=$BROWSER_TEST_AGENT_IMAGE
    docker buildx rm "$builder"
    builder=
fi

# Keep the race binary compatible with the runtime image's glibc.
cache="$PWD/state/.e2e-go-cache"
mkdir -p "$cache"
docker run --rm --platform "$platform" --cpus=3 --memory=3500m --memory-swap=3500m \
    --user "$(id -u):$(id -g)" --workdir /src \
    --mount "type=bind,source=$PWD,target=/src,readonly" \
    --mount "type=bind,source=$cache,target=/cache" \
    --mount "type=bind,source=$work,target=/out" \
    --env GOWORK=off --env GOTOOLCHAIN=local --env CGO_ENABLED=1 \
    --env GOMODCACHE=/cache/mod --env GOCACHE=/cache/build \
    golang:1.26.7-bookworm sh -ec '
        test "$(cat /sys/fs/cgroup/cpu.max)" = "300000 100000"
        test "$(cat /sys/fs/cgroup/memory.max)" = "3670016000"
        test "$(cat /sys/fs/cgroup/memory.swap.max)" = "0"
        go test -mod=readonly -race -tags=integration -c -o /out/browser-mission.test ./tests
    '

stack_started=true
compose up --wait --wait-timeout 90 agent
compose run --rm --no-deps -T mission | tee "$results/mission.log"
