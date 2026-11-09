# Commander browser agent

A [Lightpanda](https://lightpanda.io/) browser agent for
[DEFOS Commander v0.4.1](https://github.com/defos-ai/commander).

- `browse` — navigate and interact with websites; return findings and evidence.
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

Docker includes Lightpanda `1.0.2`. Native runs need that version on `PATH` or an
absolute `LIGHTPANDA_PATH`; Linux needs glibc, and Windows needs Docker or WSL.

## License

Agent source: [MIT](LICENSE). See [third-party licenses and notices](third_party/).
