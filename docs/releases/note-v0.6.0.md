# Release v0.6.0

Status: Pending release.

## browser-agent v0.6.0

Changes since browser-agent v0.5.1.

### Changes

- The agent and its Compose demo now target Commander v0.9.0.
- When the model provider or name is missing, `browse` asks through ordinary
  Commander questions. It checks model access before saving the choices as
  reusable defaults. An invalid selection can be corrected in up to three
  total attempts. API keys continue to use managed secrets.
- `browse` and `login` now load external stylesheets in Lightpanda, so page
  scripts can use the element sizes and positions those styles define.
- Session close recognizes a confirmed clean shutdown even when the Hermes
  worker exits before the agent reads its final acknowledgment.

### Upgrade

Use Commander v0.9.0 with this agent. To skip the provider and model questions
on fresh sessions, set `BROWSER_MODEL_PROVIDER` and `BROWSER_MODEL_NAME`.
For a custom OpenAI-compatible provider, setup also asks for its exact Chat
Completions endpoint. Existing managed settings must match the validated
choices. Update them in Commander when switching configurations.
