# Commander browser agent

A [Hermes](https://github.com/NousResearch/hermes-agent) and
[Lightpanda](https://lightpanda.io/) browser agent for
[DEFOS Commander v0.4.1](https://github.com/defos-ai/commander).

- `browse` — navigate and interact with websites; return protected final text.
- `login` — sign in with managed credentials and one-time codes; reuse the session.
- `screenshot` — capture the current session as a text-only PNG.

See [Commander quick start](https://github.com/defos-ai/commander#quick-start)
and [how to connect an agent](https://github.com/defos-ai/commander/blob/main/docs/agent-packages.md#bootstrap-and-registration).

## Try it locally

```sh
docker compose up --build --wait
docker compose run --rm --no-deps playground
```

The agent asks which model to use and requests credentials through Commander's
managed secrets.

One agent container runs the Go agent, Hermes `v0.23.1` and Lightpanda `1.0.2`
as separate processes. Native runs need those packaged runtimes installed;
Windows hosts use Docker or WSL.

## License

Agent source: [MIT](LICENSE). Commander dependencies: AGPL-3.0-only.
See [third-party licenses and notices](third_party/).
