# Maintaining release notes

Follow the [root guidance](../../AGENTS.md). Keep the [release index](README.md)
in descending version order and preserve historical notes.

Name each file `note-vMAJOR.MINOR.PATCH.md`. Start with `# Release <version>`.
Put `Status: Pending release.` above exactly one nonempty
`## browser-agent <version>` section for a planned release. For a historical
baseline, identify the source tag there. Add publication dates only when confirmed.

Write the package section for people upgrading the agent. Compare the changes
with the preceding tag and include any required configuration changes. Use
`###` for subsections and absolute URLs for links inside the package section,
since its body also appears on GitHub.

Validate the extracted body before dispatching a release.

```sh
.github/scripts/release-notes.sh docs/releases/note-v0.6.0.md 'browser-agent v0.6.0'
```
