"""Check pinned Hermes imports without constructing an agent or browser."""

import argparse
import importlib
import json
import logging
import os
from pathlib import Path
import signal
import sys
import tempfile


IDENTITY = {
    "release": "v0.23.1",
    "commit": "3b421578588243f920886e6fe79a6faa87ea026e",
    "archive_sha256": "2525046f165880c22f177f2214c8da3e26c58cb0aeab5bd09aad940556538897",
}


class Unavailable(Exception):
    pass


def prevent_runtime_work():
    attempted = [False]
    blocked = {"socket.connect", "socket.bind", "socket.sendto", "socket.sendmsg",
               "socket.getaddrinfo", "socket.gethostbyname", "socket.gethostbyaddr",
               "socket.getnameinfo", "subprocess.Popen", "os.posix_spawn",
               "os.fork", "os.forkpty", "os.exec", "os.system", "pty.spawn"}

    def audit(event, arguments):
        if event in blocked:
            # urllib3 catches this optional IPv6 capability probe. Decline it
            # without binding a socket or treating a supported import as broken.
            caller = sys._getframe(1)
            if (event == "socket.bind" and
                    caller.f_globals.get("__name__") == "urllib3.util.connection" and
                    caller.f_code.co_name == "_has_ipv6" and
                    len(arguments) == 2 and arguments[1] == ("::1", 0)):
                raise OSError("readiness does not bind sockets")
            attempted[0] = True
            raise Unavailable("runtime_activity_attempted")

    # This guards accidental Python import activity, not hostile native code.
    # It is deliberately not the worker's deployment isolation boundary.
    sys.addaudithook(audit)
    return attempted


def timed_out(_signal, _frame):
    raise Unavailable("readiness_timeout")


def check(source):
    if not sys.flags.isolated or not sys.flags.no_site:
        raise Unavailable("isolated_python_required")
    if sys.version_info[:3] != (3, 14, 10):
        raise Unavailable("python_version_mismatch")
    if not source.is_absolute() or Path(sys.prefix).resolve() != (source / ".venv").resolve():
        raise Unavailable("virtual_environment_mismatch")
    try:
        manifest = source / "commander-runtime.json"
        if manifest.stat().st_size > 4096 or json.loads(manifest.read_text()) != IDENTITY:
            raise Unavailable("source_identity_mismatch")
    except (OSError, ValueError):
        raise Unavailable("source_identity_unavailable") from None

    # -I -S prevents user Python settings and site hooks from running before the
    # checker establishes its private home and removes inherited credentials.
    os.environ.clear()
    with tempfile.TemporaryDirectory(prefix="commander-hermes-readiness-") as private:
        os.environ.update({
            "HOME": private,
            "HERMES_HOME": private,
            # Upstream respects caller-supplied temporary directories; this
            # avoids its detached scratch-pruning process during bootstrap.
            "TMPDIR": private,
            "TMP": private,
            "TEMP": private,
            "PATH": "/usr/local/bin:/usr/bin:/bin",
            "HERMES_DISABLE_LAZY_INSTALLS": "1",
            "PYTHONDONTWRITEBYTECODE": "1",
            "LIGHTPANDA_DISABLE_TELEMETRY": "true",
            "LIGHTPANDA_DISABLE_CORE_DUMP": "true",
            "ANONYMIZED_TELEMETRY": "false",
        })
        sys.dont_write_bytecode = True
        os.chdir(private)
        attempted = prevent_runtime_work()
        import site
        site.main()
        import importlib.metadata
        if importlib.metadata.version("browser-harness") != "0.1.13":
            raise Unavailable("browser_harness_version_mismatch")
        sys.path.insert(0, str(source))
        for module in ("ssl", "sqlite3", "bz2", "lzma", "ctypes", "readline",
                       "run_agent", "tools.browser_use_cli", "tools.browser_vault_tool",
                       "browser_harness.helpers"):
            importlib.import_module(module)
            if attempted[0]:
                raise Unavailable("runtime_activity_attempted")


def main():
    # Only fixed readiness metadata reaches stdout. Imported runtime output and
    # native writes to stdout/stderr are discarded rather than buffered.
    output = os.dup(1)
    with open(os.devnull, "wb") as null:
        os.dup2(null.fileno(), 1)
        os.dup2(null.fileno(), 2)
    logging.disable(logging.CRITICAL)
    signal.signal(signal.SIGALRM, timed_out)
    signal.alarm(20)
    try:
        parser = argparse.ArgumentParser(add_help=False)
        parser.add_argument("--source", default="/opt/hermes")
        args = parser.parse_args()
        check(Path(args.source))
    except Unavailable as error:
        result = {"status": "unavailable", "reason": str(error)}
    except (Exception, SystemExit):
        result = {"status": "unavailable", "reason": "runtime_import_failed"}
    else:
        result = {"status": "ready", "hermes": IDENTITY["release"],
                  "commit": IDENTITY["commit"], "python": "3.14.10", "browser_harness": "0.1.13"}
    finally:
        signal.alarm(0)
    if result["status"] != "ready":
        result["remedy"] = "Use the packaged Hermes runtime or rebuild the browser agent image."
    try:
        os.write(output, (json.dumps(result, sort_keys=True) + "\n").encode())
    finally:
        os.close(output)
    return 0 if result["status"] == "ready" else 1


if __name__ == "__main__":
    sys.exit(main())
