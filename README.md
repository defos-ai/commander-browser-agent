# Commander browser agent

A model-driven Lightpanda browser agent with reusable sessions for
[Commander](https://github.com/defos-ai/commander).

`browse` accepts an objective, navigates and interacts with a website using the
agent's own model, and returns structured findings with extracted evidence.
Findings and evidence are also uploaded as JSON artifacts. Browser state stays
in an isolated Commander session for reuse and inspect/verify/close controls.
Completion requires extracted evidence and findings that cite it.
Managed `login`, clarification questions, checkpoints and `screenshot` are planned
next. Screenshots will use Lightpanda's text-only PNG renderer.

Static HTML checks and crawling belong to the
[web agent](https://github.com/defos-ai/commander-web-agent).

## Try it locally

Requires Docker Compose. The image includes Lightpanda `1.0.2`; the demo uses
Commander `v0.3.0` host images.

```sh
export BROWSER_MODEL_URL=https://your-model-provider.example/v1/chat/completions
export BROWSER_MODEL_NAME=your-tool-calling-model
docker compose up --build --wait
docker compose run --rm --no-deps playground
```

Use a model endpoint supporting Chat Completions function calls. The exact URL
and model name are required; discovery reports missing configuration without
calling the model. HTTPS is required except for local HTTP endpoints. Compose
passes these two settings to the agent. Supply the model key through Commander's
managed secret named `browser-model-api-key`; the agent requests it when work
starts. Credentials are never supplied through action input or model prompts.
No demo mission is imported yet.

Open [Admin](http://localhost:4285) to inspect the registered agent and sessions.
Add `http://localhost:4284`; get its credentials with:

```sh
docker compose exec api commander-api credentials
```

Use `/exit` to close Playground and `docker compose down` to stop the stack.

For example, a controller can dispatch `browse` with:

```json
{
  "objective": "Find the support contact and summarize the available options.",
  "url": "https://example.com/",
  "engine": "lightpanda",
  "session_scope": "2e2ca5a5-c552-4ab4-90a7-62807dcb3f50"
}
```

The controller supplies the session binding. Reuse adds its `session_id` within
the same scope. Navigation stays on the starting URL's exact hostname unless
`allowed_hosts` explicitly lists additional hosts; ports may differ, while
subdomains and `www` variants are separate hosts. Model operations use observed
element IDs. Password and other credential fields are excluded from this action.
URL query strings/fragments and form values are omitted from observations.

Defaults are 12 steps, 120 seconds and 8,192 reserved completion tokens.
`max_steps` (1–30), `timeout_seconds` (5–180) and `max_completion_tokens`
(1,024–32,768) may lower or raise those limits. Each model call reserves up to
1,024 completion tokens, independent of reported usage; input and response sizes
are bounded separately. This allowance does not represent a provider price or a
total input-token quota. Step/token exhaustion returns a partial report; timeouts
and cancellation fail the execution. An unconfirmed browser operation stops work
and asks for session inspection before retrying.

## Connect to Commander

Agents need the controller's bus and artifact storage, never its database.

```sh
BUS_URL=/shared/bus STORAGE_URL=/shared/storage go run ./cmd
```

FS paths must point to the same shared directories as the controller. Distributed
deployments use RabbitMQ (`BUS_URL=amqp://…` or `amqps://…`) and S3-compatible
artifact storage (`STORAGE_URL=s3://bucket`), with service credentials supplied
through the deployment environment.

Native runs need
[Lightpanda 1.0.2](https://github.com/lightpanda-io/browser/releases/tag/1.0.2) on
`PATH`, or set `LIGHTPANDA_PATH` to its absolute executable path. Discovery checks
the installed version before reporting readiness. Linux builds require glibc;
Windows hosts can use the Docker image or WSL. The image disables Lightpanda
telemetry and crash core dumps by default.

Session-aware actions share browser state through Commander sessions. Each
session belongs to one running agent and explicitly selects `lightpanda`.
Authentication stays within that session. Commander
can inspect, verify or close it; a confirmed close interrupts active work and
removes the browser profile. Agent shutdown retains the reference as lost;
replacement requires explicit closure or controller-owned abandonment.
Native reusable sessions currently require Linux or macOS; Windows hosts use
Docker or WSL for this capability.

Run `go run ./cmd --help` for settings. A spawner sets
`COMMANDER_AGENT_ID_SUFFIX=0123abcd`, producing `browser-agent-0123abcd`.
`--agent-id-suffix` overrides that environment variable. Use eight lowercase hex
characters. Omit both settings or pass `--agent-id-suffix=` for a fresh ID.
Never use the same suffix for two running instances of this type.

## Develop and release

This repository follows [commander-example-agent](https://github.com/defos-ai/commander-example-agent).
The agent type lives in [agent.go](agent.go), the action factory in [actions/](actions/),
and browser runtime/session support in [browser/](browser/).
Implementation and integration-test guidance are in [AGENTS.md](AGENTS.md).

```sh
go test -race -count=1 -timeout=3m ./... ./.github/tests
go vet ./... ./.github/scripts ./.github/tests
```

Run **Actions → Make release** with `v0.2.1`.
It publishes native binaries for Linux/macOS/Windows on amd64/arm64, the existing
`ghcr.io/defos-ai/commander-browser-agent` image, and `agent.yaml` containing binary
checksums, the image digest and static responsibilities/action descriptions from
[description.go](description.go), using the framework's manifest template. The agent
depends on published Commander modules at `v0.3.0`; agent and Commander release
versions are independent. Grant this repository Actions write access to the
existing GHCR package before releasing.
Complete releases promote Docker `stable` and GitHub `Latest` when they are the
newest version.

Agent source: [MIT](LICENSE). Commander framework and provider dependencies
remain AGPL-3.0-only. Bundled Lightpanda is AGPL-3.0-or-later; its license,
attribution and font notices are in `/usr/share/doc/lightpanda` in the image.
The image's attribution links to the pinned upstream source. Dependency license
requirements apply to combined builds.
