# Commander browser agent

A model-driven Lightpanda browser agent with reusable sessions for
[Commander](https://github.com/defos-ai/commander).

`browse` accepts an objective, navigates and interacts with a website using the
agent's own model, and returns structured findings with extracted evidence.
Findings and evidence are also uploaded as JSON artifacts. Browser state stays
in an isolated Commander session for reuse and inspect/verify/close controls.
Completion requires extracted evidence and findings that cite it.
Model setup uses Commander's questions and managed secrets. During browsing, the
agent can ask non-secret clarification questions and continue with the saved
answers. `login` uses managed username/password credentials and retains the same
browser for later work. Checkpoints and `screenshot` are planned next.
Screenshots will use Lightpanda's text-only PNG renderer.

Static HTML checks and crawling belong to the
[web agent](https://github.com/defos-ai/commander-web-agent).

## Try it locally

Requires Docker Compose. The image includes Lightpanda `1.0.2`; the demo uses
Commander `v0.3.0` host images.

```sh
docker compose up --build --wait
docker compose run --rm --no-deps playground
```

Compose does not automatically forward model API keys. Enter the selected key
in Playground's masked setup, or explicitly pass that one host variable:

```sh
docker compose run --rm --no-deps -e OPENAI_API_KEY playground
```

On first work in a browser session, the agent asks for a provider and exact
tool-calling model ID/version through Commander's question flow. Providers are
OpenAI, Anthropic, Google Gemini, xAI, DeepSeek and Other (OpenAI-compatible),
grouped to fit Commander's four-option limit. Only Other needs an endpoint URL.
Model IDs are entered explicitly: no live catalog is requested before credentials.
Operator answers come first; the Commander client/model can answer after the
framework's waiting window. The unconfigured browser model cannot answer its own
setup questions.

Then the agent requests that provider's managed secret. Playground already shares
standard provider keys under these names when API access is configured:

| Provider | Managed secret |
| --- | --- |
| OpenAI | `OPENAI_API_KEY` |
| Anthropic | `ANTHROPIC_API_KEY` |
| Google Gemini | `GEMINI_API_KEY` |
| xAI | `XAI_API_KEY` |
| DeepSeek | `DEEPSEEK_API_KEY` |
| Other | Managed name derived from its exact endpoint |

Custom managed keys use `browser-model-api-key-` followed by the endpoint URL's
full hexadecimal SHA256; the running agent requests the exact
name. Standard provider keys are never automatically sent to custom endpoints.
Provider/model selections stay in memory for that Commander session. Reuse skips
setup questions; credentials are requested again for each execution through
Commander's leased secret cache. Already stored keys need no new value entry.
Changing a session's provider, endpoint or model requires a new session.

The agent does not read model configuration or API keys from environment
variables. New sessions obtain provider/model choices through questions; standard
endpoints are fixed and Other asks for the exact Chat Completions URL (HTTPS, or
local HTTP). Existing model environment variables do not bypass setup or secret
requests. Browser/runtime and Commander transport settings remain separate.
Keys never belong in action input, question answers, model prompts, logs or
artifacts.

Setup has a separate ten-minute bound and remains subject to the task's deadline.
Commander v0.3.0 is retained, using its existing `RequestSecret` exchange without a
default-value argument.
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
  "session_scope": "2e2ca5a5-c552-4ab4-90a7-62807dcb3f50"
}
```

The agent always uses Lightpanda; no engine input is needed. The controller
supplies the session binding from these action fields:

- `session_scope` is a caller-chosen UUID identifying a logical browser slot.
  Keep it across related tasks. It is required by Commander's session contract.
- `session_id` identifies the concrete browser session returned as `session.id`.
  Omit it to create a session; include it with the same scope to reuse that browser,
  page and cookies. Keeping the scope alone does not automatically reuse a session.

For example, a follow-up `browse` can continue on a filtered catalog page using
the returned session ID instead of opening a fresh browser.

Navigation allows all HTTP(S) hostnames by default. Omitting `allowed_hosts`,
passing `[]` or passing `["*"]` all select that behavior. To restrict navigation,
pass exact hostnames, including the starting URL's hostname, such as
`["example.com", "support.example.com"]`. Ports may differ, while subdomains and
`www` variants are separate hosts. The `*` wildcard must stand alone; partial
wildcards such as `*.example.com` are not supported. Model operations use observed
element IDs. Password and other credential fields are excluded from this action.
URL query strings/fragments and form values are omitted from observations.

Active browsing defaults to 12 steps, 120 seconds and 8,192 reserved completion
tokens, starting after setup and credential resolution.
`max_steps` (1–30), `timeout_seconds` (5–180) and `max_completion_tokens`
(1,024–32,768) may lower or raise those limits. Each model call reserves up to
1,024 requested completion tokens, independent of reported usage; input and
response sizes are bounded separately. Provider accounting differs, including
reasoning/function-call tokens, so this allowance is neither a provider cost cap
nor a total-generation/input-token quota. Step/token exhaustion returns a partial report; timeouts
and cancellation fail the execution. An unconfirmed browser operation stops work
and asks for session inspection before retrying.

When the objective needs clarification, the model can ask up to four non-secret
questions, each with up to four choices or a free-text answer. Commander gives
the operator the waiting window first, then permits its eligible model to answer.
The browser model waits for the saved answer; it does not answer its own question.
Questions must never request passwords, credentials or authentication codes.
Use Commander's managed-secret flow for sensitive values.

Each clarification wait is bounded to three minutes and pauses the active browsing
timer without extending the task deadline. Asking still consumes a browser step
and model completion allowance. Answers are bounded to 2,000 UTF-8 bytes and stay
in the current execution's model context with the question and its choices.
Execution logs record static progress, and artifacts contain findings/evidence
without a question-and-answer transcript.

Cancellation stops the wait and further browser work; the browser remains reusable
unless it was closed or lost. Commander v0.3.0 can retain an unanswered saved
question after cancellation or timeout. Inspect or answer it through Commander;
the agent does not retract, auto-answer or restart the interrupted browsing work.

## Sign in

`login` supports a standard visible username/password POST form in the main page.
It uses trusted browser code rather than the browsing model, so model setup is not
needed to sign in. For example:

```json
{
  "url": "https://example.com/login",
  "credential_profile": "work",
  "verification_url": "https://example.com/account",
  "expected_text": "Sign out",
  "session_scope": "2e2ca5a5-c552-4ab4-90a7-62807dcb3f50"
}
```

The non-secret profile is an account alias. Omitting it asks a Commander question;
never enter a username, password or verification code as its answer. Credentials
are always requested through `action.RequestSecret`. Their managed names are
`browser-login-<profile>-<digest>-username` and the matching `-password` name, where
`digest` is the full hexadecimal SHA256 of the canonical origin, a NUL separator,
and the profile. Default ports are normalized. Names remain stable across browser
replicas but are different for other origins or account profiles. Commander v0.3.0
shares secrets by name; this convention is not an access-control policy.

Both URLs must use the same exact origin (scheme, hostname and effective port),
with HTTPS required except loopback HTTP. Credential handling blocks cross-origin
requests and rechecks the selected form before filling and issuing one browser
submission command.
After login, normal cross-site browsing remains available. The website's own
scripts can access submitted form values; use the intended, trusted login site.

`verified` means the configured visible text was found at the verification URL.
It is a site-specific check, not universal proof of an account's identity. If the
check already matches, no credentials are requested or submitted. Other outcomes
include `unverified`, `unsupported_form`, `requires_challenge` and
`submission_uncertain`; inspect the retained session before retrying uncertain
work. Login produces status metadata without page-text or credential artifacts.
One command is not an exactly-once website guarantee: the pinned Lightpanda can
internally repeat an HTTP POST after a dropped connection, below CDP interception.
The agent stops on the uncertain outcome and does not issue another submission.

Multi-step forms, verification codes, CAPTCHA and passkey flows need further
support. In particular, code challenges do not request a reusable secret on
Commander v0.3.0; transient secret requests are planned for Commander v0.4.0.

Reuse the returned `session.id` and the same scope in `browse` to keep cookies and
the retained page. Its first model-driven execution performs model setup as usual.
Owned credential buffers are cleared after use. Session protection keeps private
keyed fingerprints for redacting exact matches from subsequent browsing
observations, questions and findings; Commander manages its own secret leases.
These fingerprints remain with that session, including
after a failed login, and are cleared on confirmed close. This is bounded value
redaction, not a guarantee against arbitrary transformations by a website.

Login defaults to a 120-second active allowance, configurable with
`timeout_seconds` (5–180). Missing credential waits pause that allowance and are
individually bounded to three minutes; the outer task deadline still applies.

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
session belongs to one running agent and uses Lightpanda.
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
