# Release v0.5.1

Source: Git tag `v0.5.1` (`a80e69b`).

## browser-agent v0.5.1

### Capabilities

- `browse` uses Hermes to navigate and interact with websites in Lightpanda.
- `login` signs in with Commander-managed credentials and one-time codes.
  Later actions in the same session can use the authenticated page.
- `screenshot` captures the current page as a text-only PNG.

### Operation

The agent targets Commander v0.6.0. Its container packages Hermes v0.23.1
and Lightpanda 1.0.2. Native installations require those runtimes on the host.
