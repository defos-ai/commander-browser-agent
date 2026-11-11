"""Check the packaged Hermes runtime without starting an agent or browser."""

import contextlib
import importlib
import importlib.metadata
import io
import json
import logging
import os
from pathlib import Path
import signal
import sys
import tempfile


SOURCE = Path("/opt/hermes")
IDENTITY = {
    "release": "v0.23.1",
    "commit": "3b421578588243f920886e6fe79a6faa87ea026e",
    "archive_sha256": "2525046f165880c22f177f2214c8da3e26c58cb0aeab5bd09aad940556538897",
}


class Unavailable(Exception):
    pass


def timed_out(_signal, _frame):
    raise Unavailable("readiness_timeout")


def check():
    if sys.version_info[:3] != (3, 14, 10):
        raise Unavailable("python_version_mismatch")
    try:
        manifest = SOURCE / "commander-runtime.json"
        if manifest.stat().st_size > 4096 or json.loads(manifest.read_text()) != IDENTITY:
            raise Unavailable("source_identity_mismatch")
    except (OSError, ValueError):
        raise Unavailable("source_identity_unavailable") from None
    if importlib.metadata.version("browser-harness") != "0.1.13":
        raise Unavailable("browser_harness_version_mismatch")

    # Discovery must not load an operator's profile or inherit model credentials.
    os.environ.clear()
    with tempfile.TemporaryDirectory(prefix="commander-hermes-readiness-") as private:
        os.environ.update({
            "HOME": private,
            "HERMES_HOME": private,
            "PATH": "/usr/local/bin:/usr/bin:/bin",
            "HERMES_DISABLE_LAZY_INSTALLS": "1",
            "PYTHONDONTWRITEBYTECODE": "1",
            "LIGHTPANDA_DISABLE_TELEMETRY": "true",
            "LIGHTPANDA_DISABLE_CORE_DUMP": "true",
            "ANONYMIZED_TELEMETRY": "false",
        })
        sys.dont_write_bytecode = True
        sys.path.insert(0, str(SOURCE))
        for module in ("ssl", "sqlite3", "bz2", "lzma", "ctypes", "readline",
                       "run_agent", "tools.browser_use_cli", "tools.browser_vault_tool",
                       "browser_harness.helpers"):
            importlib.import_module(module)


def main():
    signal.signal(signal.SIGALRM, timed_out)
    signal.alarm(20)
    logging.disable(logging.CRITICAL)
    try:
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            check()
    except Unavailable as error:
        result = {"status": "unavailable", "reason": str(error)}
    except Exception:
        result = {"status": "unavailable", "reason": "runtime_import_failed"}
    else:
        result = {"status": "ready", "hermes": IDENTITY["release"],
                  "commit": IDENTITY["commit"], "python": "3.14.10", "browser_harness": "0.1.13"}
    finally:
        signal.alarm(0)
    if result["status"] != "ready":
        result["remedy"] = "Use the packaged Hermes runtime or rebuild the browser agent image."
    print(json.dumps(result, sort_keys=True))
    return 0 if result["status"] == "ready" else 1


if __name__ == "__main__":
    sys.exit(main())
