# Releases

| Version | Notes |
| --- | --- |
| [v0.6.0](note-v0.6.0.md) | Pending release. Changes since v0.5.1. |
| [v0.5.1](note-v0.5.1.md) | Source baseline from the v0.5.1 tag. |

[GitHub release history](https://github.com/defos-ai/commander-browser-agent/releases)

## Publish

Commit `note-<version>.md` with a `## browser-agent <version>` section and
link it in the table above. The workflow uses that section as the GitHub
release description.

Run **Make release** on the default branch and enter an unused agent version
in `vMAJOR.MINOR.PATCH` form. Agent and Commander versions are independent.
For example, to dispatch v0.6.0 from the CLI, run

```sh
gh workflow run make-release.yml --ref main -f version=v0.6.0
```

The workflow runs the tests and packages `agent.yaml`, `LICENSE`, and Linux
and macOS executables for amd64 and arm64. It publishes the Linux image as
`ghcr.io/defos-ai/commander-browser-agent:<version>`. The newest published
version is promoted to `stable` and GitHub Latest. [Make release](../../.github/workflows/make-release.yml)

If a run fails, rerun its failed jobs. A draft retry uses the notes from
the same commit.
