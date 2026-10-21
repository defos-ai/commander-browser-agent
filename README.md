# Commander browser agent

Render pages with Chromium, capture desktop/mobile screenshots and check rendered
accessibility for [Commander](https://github.com/defos-ai/commander) missions.

| Action | Result |
| --- | --- |
| `capture-page` | Desktop/mobile PNGs, page title and horizontal overflow findings |
| `check-accessibility` | Bounded checks for headings, image alternatives, form labels and links |

Static HTML checks and crawling belong to the
[web agent](https://github.com/defos-ai/commander-web-agent).

## Try it locally

Requires Docker Compose. The image includes Chromium; Commander hosts default to
`v0.1.0`. Set `COMMANDER_VERSION` to use another Commander release.

```sh
docker compose up --build --wait
docker compose run --rm --no-deps playground
```

Choose a model provider and enter its API key. Try:

> Run review-page for https://defos.ai and summarize the screenshots and accessibility findings.

Open [Admin](http://localhost:4285) to inspect results and download screenshots.
Add `http://localhost:4284`; get its credentials with:

```sh
docker compose exec api commander-api credentials
```

Use `/exit` to close Playground and `docker compose down` to stop the stack.

## Connect to Commander

Agents need the controller's bus and artifact storage, never its database.

```sh
BUS_URL=/shared/bus STORAGE_URL=/shared/storage go run ./cmd
```

FS paths must point to the same shared directories as the controller. Distributed
deployments use RabbitMQ (`BUS_URL=amqp://…` or `amqps://…`) and S3-compatible
artifact storage (`STORAGE_URL=s3://bucket`), with service credentials supplied
through the deployment environment.

Native runs need Chromium or Chrome. Set `CHROMIUM_PATH` to an absolute executable
path if discovery cannot find it. Native sandboxing stays enabled; the Docker
image sets `CHROMIUM_NO_SANDBOX=true` and runs as an unprivileged user.

Run `go run ./cmd --help` for settings. A spawner sets
`COMMANDER_AGENT_ID_SUFFIX=0123abcd`, producing `browser-agent-0123abcd`.
`--agent-id-suffix` overrides that environment variable. Use eight lowercase hex
characters. Omit both settings or pass `--agent-id-suffix=` for a fresh ID.
Never use the same suffix for two running instances of this type.

## Develop and release

This repository follows [commander-example-agent](https://github.com/defos-ai/commander-example-agent).
The agent type lives in [agent.go](agent.go), rendering actions in [actions/](actions/),
and the demo mission in [missions/review-page.json](missions/review-page.json).
Implementation and integration-test guidance are in [AGENTS.md](AGENTS.md).

```sh
go test -race -count=1 -timeout=3m ./... ./.github/tests
go vet ./... ./.github/scripts ./.github/tests
```

Run **Actions → Make release** with `v0.2.0` for the first standalone release.
It publishes native binaries for Linux/macOS/Windows on amd64/arm64, the existing
`ghcr.io/defos-ai/commander-browser-agent` image, and `agent.yaml` containing binary
checksums, the image digest and static responsibilities/action descriptions from
[description.go](description.go). The agent depends on published Commander modules
at `v0.1.0`; agent and Commander release versions are independent. Grant this
repository Actions write access to the existing GHCR package before releasing.
Complete releases promote Docker `stable` and GitHub `Latest` when they are the
newest version.

Agent source: [MIT](LICENSE). Commander framework and provider dependencies
remain AGPL-3.0-only. Their license requirements apply to combined builds.
