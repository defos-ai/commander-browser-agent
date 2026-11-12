"""Lend container stdio to the private bridge before loading runtime imports."""

import os
from pathlib import Path
import runpy
import sys


def main():
    if not sys.flags.isolated or not sys.flags.no_site:
        return 1
    os.umask(0o077)
    # Docker attach owns these two streams. Upstream/native child output must
    # never share them; the bridge alone owns non-inheritable FD3 and FD4.
    os.dup2(0, 3, inheritable=False)
    os.dup2(1, 4, inheritable=False)
    with open(os.devnull, "r+b", buffering=0) as null:
        for descriptor in (0, 1, 2):
            os.dup2(null.fileno(), descriptor)

    home = Path("/home/commander/state/worker")
    temporary = Path("/tmp/commander-worker")
    home.mkdir(mode=0o700, parents=True, exist_ok=True)
    temporary.mkdir(mode=0o700, exist_ok=True)
    os.environ.clear()
    os.environ.update({
        "HOME": "/home/commander",
        "HERMES_HOME": str(home),
        "HERMES_INSTALL_ROOT": "/opt/hermes",
        "TMPDIR": str(temporary), "TMP": str(temporary), "TEMP": str(temporary),
        "PATH": "/usr/local/bin:/usr/bin:/bin",
        "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8",
        "HERMES_DISABLE_LAZY_INSTALLS": "1",
        "PYTHONDONTWRITEBYTECODE": "1",
        "ANONYMIZED_TELEMETRY": "false",
        "LIGHTPANDA_DISABLE_TELEMETRY": "true",
        "LIGHTPANDA_DISABLE_CORE_DUMP": "true",
        "LIGHTPANDA_PATH": "/usr/local/bin/lightpanda",
        "BH_REQUIRE_EXISTING_DAEMON": "1",
    })
    sys.dont_write_bytecode = True
    os.chdir(home)
    # -I -S establishes stdio/environment ownership before trusted site hooks.
    import site
    site.main()
    worker = Path(__file__).with_name("worker.py")
    sys.argv[0] = str(worker)
    runpy.run_path(str(worker), run_name="__main__")
    return 0


if __name__ == "__main__":
    sys.exit(main())
