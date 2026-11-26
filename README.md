# Commander browser agent

A [Hermes](https://github.com/NousResearch/hermes-agent) and
[Lightpanda](https://lightpanda.io/) browser agent for
[DEFOS Commander v0.9.0](https://github.com/defos-ai/commander/tree/v0.9.0).

- `browse` — navigate and interact with websites; return protected final text.
- `login` — sign in with managed credentials and one-time codes; reuse the session.
- `screenshot` — capture the current session as a text-only PNG.

See [Commander quick start](https://github.com/defos-ai/commander/tree/v0.9.0#quick-start)
and [how to connect an agent](https://github.com/defos-ai/commander/blob/v0.9.0/docs/agent-packages.md#bootstrap-and-registration).

## Try it locally

```sh
export BROWSER_MODEL_PROVIDER=openai
export BROWSER_MODEL_NAME=chat-latest
docker compose up --build --wait
docker compose run --rm --no-deps playground
```

Provider/model variables are optional. Missing choices use regular Commander
questions, with up to three setup attempts. Successful choices become reusable
defaults after a connection check. API keys use managed secrets.
See [AGENTS.md](AGENTS.md#intelligence-and-model-access) for custom providers.

One agent container runs the Go agent, Hermes `v0.23.1` and Lightpanda `1.0.2`
as separate processes. Native runs need those packaged runtimes installed;
Windows hosts use Docker or WSL.

[Release notes](docs/releases/README.md)

## License

Agent source: [MIT](LICENSE). Commander dependencies: AGPL-3.0-only.
See [third-party licenses and notices](third_party/).
