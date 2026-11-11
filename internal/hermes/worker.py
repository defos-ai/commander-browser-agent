"""Private Commander host protocol around the pinned, unmodified Hermes runtime.

Native browser code is a host-execution capability, not a Python sandbox.
Admission fences an entire invocation. This worker is not an action backend.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import importlib.metadata
import json
import logging
import os
from pathlib import Path
import queue
import secrets
import shutil
import sys
import threading
import time
from urllib.parse import urlsplit
import ipaddress


MAX_FRAME = 1 << 20
MAX_TEXT = 64 << 10
MAX_NATIVE_RESULT = 128 << 10
IDENTITY = {"release": "v0.23.1", "commit": "3b421578588243f920886e6fe79a6faa87ea026e",
            "archive_sha256": "2525046f165880c22f177f2214c8da3e26c58cb0aeab5bd09aad940556538897"}


class Refused(Exception):
    pass


def bounded_text(value, maximum):
    if not isinstance(value, str) or len(value.encode("utf-8")) > maximum or "\x00" in value:
        raise Refused("invalid_request")
    return value


def frame_id(value):
    return bounded_text(value, 128) if isinstance(value, str) and value else ""


class Run:
    def __init__(self, identifier, request):
        self.id, self.request = identifier, request
        self.cancelled = threading.Event()
        self.finished = threading.Event()
        self.deadline = time.monotonic() + request["timeout_seconds"]
        self.sequence, self.questions, self.in_native = 0, 0, 0
        self.halted = ""
        self.agent = self.thread = None
        self.key = request.pop("key")

    def redact(self, value):
        return value.replace(self.key, "[redacted]") if self.key else value


class Worker:
    def __init__(self, args, output):
        self.args, self.output = args, output
        self.write_lock = threading.Lock()
        self.lock = threading.RLock()
        self.native_lock = threading.Lock()
        self.active = None
        self.pending = {}
        self.handlers = {}
        self.closing = False
        self.tainted = False
        self.session_name = "commander_" + hashlib.sha256(args.session.encode()).hexdigest()[:24]
        self.owned = {}
        self.owned_groups = set()
        self.supervisors = {}

    def send(self, kind, identifier, payload):
        raw = json.dumps({"version": 1, "kind": kind, "id": identifier, "payload": payload},
                         separators=(",", ":"), ensure_ascii=True).encode() + b"\n"
        if len(raw) > MAX_FRAME:
            raise Refused("frame_too_large")
        with self.write_lock:
            self.output.write(raw)
            self.output.flush()

    def initialize(self):
        if sys.version_info[:3] != (3, 14, 10):
            raise Refused("python_version_mismatch")
        source, home, lightpanda = (Path(self.args.source), Path(self.args.home), Path(self.args.lightpanda))
        if not all(path.is_absolute() for path in (source, home, lightpanda)) or not self.args.session:
            raise Refused("invalid_worker_paths")
        marker = source / "commander-runtime.json"
        if marker.stat().st_size > 4096 or json.loads(marker.read_text()) != IDENTITY:
            raise Refused("source_identity_mismatch")
        if importlib.metadata.version("browser-harness") != "0.1.13":
            raise Refused("browser_harness_version_mismatch")
        if not lightpanda.is_file() or not os.access(lightpanda, os.X_OK):
            raise Refused("lightpanda_unavailable")
        home.mkdir(mode=0o700, parents=True, exist_ok=True)
        if home.is_symlink() or home.stat().st_uid != os.getuid() or home.stat().st_mode & 0o077:
            raise Refused("private_home_required")
        bin_dir = home / "bin"
        bin_dir.mkdir(mode=0o700, exist_ok=True)
        (bin_dir / "lightpanda").symlink_to(lightpanda)
        os.environ.clear()
        os.environ.update({"HOME": str(home), "HERMES_HOME": str(home), "HERMES_INSTALL_ROOT": str(source),
                           "HERMES_DISABLE_LAZY_INSTALLS": "1", "PYTHONDONTWRITEBYTECODE": "1",
                           "ANONYMIZED_TELEMETRY": "false", "LIGHTPANDA_DISABLE_TELEMETRY": "true",
                           "LIGHTPANDA_DISABLE_CORE_DUMP": "true",
                           "PATH": str(bin_dir) + ":" + str(Path(sys.executable).parent) + ":/usr/local/bin:/usr/bin:/bin"})
        sys.dont_write_bytecode = True
        os.chdir(home)
        plugin = home / "plugins" / "commander-browser"
        shutil.copytree(Path(__file__).with_name("plugin"), plugin)
        (home / "config.yaml").write_text(json.dumps({
            "browser": {"cloud_provider": "local", "engine": "lightpanda", "backend": "browser-use", "inactivity_timeout": 600},
            "plugins": {"enabled": ["commander-browser"], "entries": {
                "commander-browser": {"granted_capabilities": ["tools.override"]}}},
            "tools": {"tool_search": {"enabled": "off"}},
            "model": {"streaming": False, "context_length": 128000},
            "memory": {"enabled": False}, "security": {"redact_secrets": True},
        }))
        sys.path[:0] = [str(Path(__file__).parent), str(source)]
        from support import configure
        configure(self)
        from run_agent import AIAgent
        from hermes_cli.plugins import discover_plugins
        discover_plugins()
        self.agent_type = AIAgent
        self.verify_handlers()

    def register_handler(self, name, handler):
        self.handlers[name] = handler

    def verify_handlers(self):
        from tools.registry import registry
        if set(self.handlers) != {"browser_exec", "terminal"}:
            raise Refused("required_overrides_unavailable")
        for name, handler in self.handlers.items():
            entry = registry.get_entry(name)
            if entry is None or entry.handler is not handler:
                raise Refused("required_overrides_unavailable")

    def request(self, payload):
        if not isinstance(payload, dict):
            raise Refused("invalid_request")
        request = dict(payload)
        if request.get("provider") != "openai":
            raise Refused("unsupported_provider")
        for name, maximum in (("objective", 16384), ("model", 200), ("base_url", 2048)):
            value = bounded_text(request.get(name), maximum)
            if not value.strip():
                raise Refused("invalid_request")
        endpoint = urlsplit(request["base_url"])
        try:
            local = endpoint.hostname == "localhost" or ipaddress.ip_address(endpoint.hostname or "").is_loopback
        except ValueError:
            local = False
        if (not endpoint.hostname or endpoint.username is not None or endpoint.password is not None or
                endpoint.query or endpoint.fragment or endpoint.scheme not in {"http", "https"} or
                endpoint.scheme == "http" and not local):
            raise Refused("invalid_endpoint")
        for name, maximum in (("max_iterations", 30), ("max_tokens", 16384), ("timeout_seconds", 600)):
            value = request.get(name)
            if type(value) is not int or not 1 <= value <= maximum:
                raise Refused("invalid_request")
        try:
            encoded = bounded_text(request.pop("api_key"), 12000)
            key = base64.b64decode(encoded, validate=True)
            if not 1 <= len(key) <= 8192:
                raise ValueError()
            key = key.decode("utf-8")
        except (KeyError, ValueError, UnicodeDecodeError):
            raise Refused("invalid_key") from None
        if "\r" in key or "\n" in key or "\x00" in key or any(key in request[name] for name in ("objective", "model", "base_url")):
            raise Refused("invalid_key")
        request["key"] = key
        return request

    def event(self, run, payload, *, wait=True):
        if run.cancelled.is_set() or run.finished.is_set() or self.closing:
            raise Refused("execution_cancelled")
        identifier = run.id + ":" + secrets.token_hex(12)
        waiter = queue.Queue(maxsize=1)
        if wait:
            with self.lock:
                self.pending[identifier] = waiter
        try:
            self.send("event", identifier, payload)
            if not wait:
                return {}
            while not run.cancelled.is_set() and not self.closing:
                remaining = run.deadline - time.monotonic()
                if remaining <= 0:
                    raise Refused("execution_timeout")
                try:
                    return waiter.get(timeout=min(remaining, 0.1))
                except queue.Empty:
                    pass
            raise Refused("execution_cancelled")
        finally:
            with self.lock:
                self.pending.pop(identifier, None)

    def halt(self, status):
        with self.lock:
            run = self.active
            if run is None:
                return
            run.halted = run.halted or status
            agent = run.agent
        if agent is not None:
            try:
                agent.interrupt(hard_cancel=True, tool_reason="browser execution stopped")
            except Exception:
                pass

    def browser(self, handler, args):
        with self.native_lock:
            with self.lock:
                run = self.active
            if run is None or run.halted or run.cancelled.is_set() or self.closing:
                return json.dumps({"error": "Browser execution is unavailable."})
            try:
                if not isinstance(args, dict):
                    raise Refused("invalid_browser_request")
                code = bounded_text(args.get("code"), MAX_TEXT)
                if not code.strip() or args.get("local") or run.sequence >= run.request["max_iterations"]:
                    raise Refused("invalid_browser_request")
                run.sequence += 1
                reply = self.event(run, {"kind": "admit", "sequence": run.sequence, "tool": "browser_exec"})
                if reply.get("approved") is not True:
                    raise Refused("admission_refused")
                remaining = int(run.deadline - time.monotonic())
                if remaining < 5 or run.cancelled.is_set() or run.halted:
                    raise Refused("execution_timeout")
                self.event(run, {"kind": "progress", "sequence": run.sequence,
                                 "tool": "browser_exec", "text": "started"}, wait=False)
            except Exception:
                self.halt("failed")
                return json.dumps({"error": "Browser admission was refused."})
            run.in_native += 1
            try:
                # The public registry supplied this exact native handler. Do not
                # redispatch the overridden name or forward model session routing.
                result = handler({"code": code, "session": self.session_name,
                                  "timeout_s": min(remaining, 300)}, task_id=self.session_name)
                if run.cancelled.is_set():
                    raise Refused("execution_cancelled")
                encoded = result if isinstance(result, str) else json.dumps(result)
                encoded = run.redact(encoded)
                if len(encoded.encode("utf-8")) > MAX_NATIVE_RESULT:
                    raise Refused("browser_output_limit")
                parsed = json.loads(encoded)
                if isinstance(parsed, dict) and (parsed.get("success") is False or "error" in parsed):
                    raise Refused("browser_operation_unconfirmed")
                self.event(run, {"kind": "progress", "sequence": run.sequence,
                                 "tool": "browser_exec", "text": "finished"}, wait=False)
                return encoded
            except Exception:
                self.halt("uncertain")
                return json.dumps({"error": "Browser execution could not be confirmed."})
            finally:
                run.in_native -= 1
                self.remember_owned()

    def clarify(self, run, questions):
        answers = {}
        try:
            if not isinstance(questions, list) or not 1 <= len(questions) <= 4:
                raise Refused("invalid_question")
            for question in questions:
                if run.questions >= 4 or not isinstance(question, dict):
                    raise Refused("question_limit")
                text = run.redact(bounded_text(question.get("question"), 2000)).strip()
                options = question.get("choices_offered") or []
                if not text or not isinstance(options, list) or len(options) > 4 or question.get("multi_select"):
                    raise Refused("invalid_question")
                options = [run.redact(bounded_text(option, 200)).strip() for option in options]
                if any(not option for option in options):
                    raise Refused("invalid_question")
                run.questions += 1
                reply = self.event(run, {"kind": "question", "text": text, "options": options})
                answer = run.redact(bounded_text(reply.get("answer"), 2000)).strip()
                if not answer:
                    raise Refused("question_cancelled")
                answers[question["qid"]] = answer
            return {"answers": answers, "outcome": "submitted"}
        except Exception:
            self.halt("failed")
            return {"answers": {}, "outcome": "cancelled"}

    def execute(self, run):
        result = {"status": "failed"}
        agent = None
        try:
            self.verify_handlers()
            agent = self.agent_type(base_url=run.request["base_url"], api_key=run.key, provider="openai",
                                    api_mode="chat_completions", model=run.request["model"],
                                    max_iterations=run.request["max_iterations"], max_tokens=run.request["max_tokens"],
                                    run_budget_seconds=run.request["timeout_seconds"],
                                    enabled_toolsets=["commander-browser", "clarify"], quiet_mode=True,
                                    verbose_logging=False, save_trajectories=False, skip_context_files=True,
                                    skip_memory=True, skip_background_review=True, session_id=self.session_name,
                                    clarify_callback=lambda questions: self.clarify(run, questions))
            run.agent = agent
            self.verify_handlers()
            if agent.valid_tool_names != {"browser_exec", "terminal", "clarify"}:
                raise Refused("unexpected_tool_catalog")
            if run.cancelled.is_set():
                raise Refused("execution_cancelled")
            raw = agent.run_conversation(run.request["objective"], task_id=self.session_name)
            if run.in_native:
                self.tainted = True
                run.halted = "uncertain"
            count = raw.get("api_calls", 0)
            if type(count) is not int or not 0 <= count <= 1000000:
                raise Refused("invalid_usage")
            status = run.halted or ("interrupted" if run.cancelled.is_set() or raw.get("interrupted") else
                                    "failed" if raw.get("failed") else "completed" if raw.get("completed") else "incomplete")
            text = run.redact(raw.get("final_response") or "")
            text = text.encode("utf-8", "replace")[:MAX_TEXT].decode("utf-8", "ignore")
            result = {"status": status, "text": text, "model_requests": count}
        except Exception:
            result = {"status": run.halted or ("interrupted" if run.cancelled.is_set() else "failed")}
        finally:
            if agent is not None:
                try:
                    agent.release_clients()
                except Exception:
                    self.tainted = True
                    result = {"status": "uncertain"}
            run.agent = None
            run.key = ""
            with self.lock:
                run.finished.set()
                self.send("result", run.id, result)
                if self.active is run:
                    self.active = None

    def cancel(self):
        with self.lock:
            run = self.active
            if run is not None:
                run.cancelled.set()
                agent = run.agent
            else:
                agent = None
        if agent is not None:
            try:
                agent.interrupt(hard_cancel=True, tool_reason="browser execution cancelled")
            except Exception:
                self.tainted = True

    def remember_owned(self):
        try:
            import psutil
            from hermes_cli.process_identity import is_owner_death_supervisor, ledger_entries
            processes = psutil.Process().children(recursive=True)
            for entry in ledger_entries(project_root=Path(self.args.source), verified_only=True):
                if entry.get("spawner_pid") == os.getpid():
                    try:
                        processes.append(psutil.Process(entry["pid"]))
                    except psutil.NoSuchProcess:
                        pass  # The verified process may exit before this snapshot.
            for process in processes:
                try:
                    birth = process.create_time()
                    supervisor = (process.ppid() == os.getpid() and
                                  is_owner_death_supervisor(process.cmdline()))
                    group = os.getpgid(process.pid)
                    with self.lock:
                        self.owned[process.pid] = birth
                        if supervisor:
                            self.supervisors[process.pid] = birth
                        elif group > 1 and group != os.getpgrp():
                            self.owned_groups.add(group)
                except (psutil.NoSuchProcess, ProcessLookupError):
                    pass
        except Exception:
            self.tainted = True

    def close(self):
        self.closing = True
        with self.lock:
            run = self.active
        self.cancel()
        if run is not None and run.thread is not None:
            run.thread.join(5)
            if run.thread.is_alive():
                return False
        self.remember_owned()
        try:
            from tools.browser_session_lifecycle import cleanup_all_browsers
            from tools.browser_use_cli import stop_harness_daemons
            cleanup_all_browsers()
            stop_harness_daemons()
            import psutil
            from hermes_cli.process_identity import is_owner_death_supervisor
            deadline = time.monotonic() + 3
            supervisors_stopped = False
            while time.monotonic() < deadline:
                self.remember_owned()
                alive = []
                with self.lock:
                    owned = dict(self.owned)
                    groups = set(self.owned_groups)
                for pid, birth in owned.items():
                    try:
                        process = psutil.Process(pid)
                        if process.create_time() == birth:
                            alive.append(process)
                    except psutil.NoSuchProcess:
                        pass
                groups_gone = True
                for group in groups:
                    try:
                        os.killpg(group, 0)
                        groups_gone = False
                    except ProcessLookupError:
                        pass
                if not alive and groups_gone:
                    return not self.tainted
                # Hermes intentionally leaves this idle helper resident for 30s.
                # Stop only our exact direct child, after every other recorded
                # process and process group is gone. Never ignore it or change
                # upstream's private idle timer. A restart remains uncertain.
                if alive and groups_gone and not supervisors_stopped:
                    only_supervisors = all(
                        self.supervisors.get(process.pid) == owned[process.pid] and
                        process.create_time() == owned[process.pid] and process.ppid() == os.getpid() and
                        is_owner_death_supervisor(process.cmdline()) for process in alive)
                    if only_supervisors:
                        supervisors_stopped = True
                        for process in alive:
                            process.terminate()
                        psutil.wait_procs(alive, timeout=1)
                time.sleep(0.05)
        except Exception:
            pass
        return False

    def accept(self, frame):
        if (not isinstance(frame, dict) or set(frame) != {"version", "kind", "id", "payload"} or
                type(frame["version"]) is not int or frame["version"] != 1 or not isinstance(frame["payload"], dict)):
            raise Refused("invalid_frame")
        kind, identifier, payload = frame["kind"], frame_id(frame["id"]), frame["payload"]
        if not identifier:
            raise Refused("invalid_frame")
        if kind == "reply":
            with self.lock:
                waiter = self.pending.get(identifier)
                if waiter is not None:
                    try:
                        waiter.put_nowait(payload)
                    except queue.Full:
                        raise Refused("duplicate_reply") from None
            return True
        if kind == "cancel":
            with self.lock:
                if self.active is not None and self.active.id == identifier:
                    self.cancel()
            return True
        if kind == "close":
            clean = self.close()
            self.send("closed", identifier, {"clean": clean})
            return False
        if kind != "run":
            raise Refused("invalid_frame")
        with self.lock:
            if self.active is not None or self.closing or self.tainted:
                raise Refused("worker_unavailable")
            run = Run(identifier, self.request(payload))
            self.active = run
            run.thread = threading.Thread(target=self.execute, args=(run,), name="commander-browser-run", daemon=True)
            run.thread.start()
        return True


def main():
    # The inherited protocol is separate from every upstream print/log sink.
    with open(os.devnull, "wb") as null:
        os.dup2(null.fileno(), 1)
        os.dup2(null.fileno(), 2)
    os.set_inheritable(3, False)
    os.set_inheritable(4, False)
    incoming, outgoing = os.fdopen(3, "rb", buffering=0), os.fdopen(4, "wb", buffering=0)
    parser = argparse.ArgumentParser()
    for name in ("source", "session", "lightpanda", "home"):
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    logging.disable(logging.CRITICAL)
    worker = Worker(args, outgoing)
    try:
        worker.initialize()
        worker.send("ready", "", {"hermes": IDENTITY["release"]})
        while True:
            line = incoming.readline(MAX_FRAME + 1)
            if not line:
                worker.close()
                return 0
            if len(line) > MAX_FRAME or not line.endswith(b"\n"):
                raise Refused("invalid_frame")
            frame = json.loads(line)
            try:
                if not worker.accept(frame):
                    return 0
            except Refused as error:
                worker.send("error", frame.get("id", ""), {"code": str(error)})
    except BaseException:
        try:
            worker.send("error", "", {"code": "worker_unavailable"})
            worker.close()
        except BaseException:
            pass
        return 1


if __name__ == "__main__":
    sys.exit(main())
