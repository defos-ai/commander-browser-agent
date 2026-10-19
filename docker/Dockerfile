# syntax=docker/dockerfile:1
FROM golang:1.26.7-bookworm AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath -buildvcs=false -o /out/commander-browser-agent ./cmd

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates chromium fonts-liberation \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 1000 commander \
    && useradd --uid 1000 --gid 1000 --create-home commander \
    && mkdir -p /state \
    && chown 1000:1000 /state
COPY --from=build /out/commander-browser-agent /usr/local/bin/commander-browser-agent
# Chromium needs a writable real user's home. Docker supplies the outer sandbox;
# native launches keep Chromium's sandbox enabled unless explicitly configured.
ENV HOME=/home/commander CHROMIUM_PATH=/usr/bin/chromium CHROMIUM_NO_SANDBOX=true \
    BUS_URL=file:///state/bus STORAGE_URL=file:///state/storage
USER 1000:1000
WORKDIR /state
ENTRYPOINT ["/usr/local/bin/commander-browser-agent"]
