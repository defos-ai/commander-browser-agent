"""Host-owned plugin binding; no Hermes core registrations are changed here."""

_bridge = None


def configure(bridge):
    global _bridge
    if _bridge is not None:
        raise RuntimeError("worker_already_configured")
    _bridge = bridge


def bridge():
    if _bridge is None:
        raise RuntimeError("worker_unavailable")
    return _bridge
