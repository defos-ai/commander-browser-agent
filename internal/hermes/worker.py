"""Private Commander host protocol around the pinned, unmodified Hermes runtime.

Native browser code is a host-execution capability, not a Python sandbox.
Admission fences an entire invocation. This worker is not an action backend.
"""

from __future__ import annotations

import argparse
import base64
import contextvars
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
        self.request_threads = set()
        self.key = request.pop("key")

    def redact(self, value):
        return value.replace(self.key, "[redacted]") if self.key else value


class Worker:
    def __init__(self, args, output):
        self.args, self.output = args, output
        self.write_lock = threading.Lock()
        self.lock = threading.RLock()
        self.native_lock = threading.Lock()
        self.execution = contextvars.ContextVar("commander_browser_execution", default=None)
        self.active = None
        self.pending = {}
        self.handlers = {}
        self.closing = False
        self.tainted = False
        self.session_name = "commander_" + hashlib.sha256(args.session.encode()).hexdigest()[:24]
        self.owned = {}
        self.owned_groups = set()
        self.supervisors = {}
        self.browser_pin = None
        self.browser_attempted = False
        self.lost = False

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
        # Pinned internal observation, not a general Hermes cancellation API:
        # this exact nonstreaming request worker can outlive an interrupted turn.
        from agent.chat_completion_nonstream import _NonStreamRequest
        self.request_type = _NonStreamRequest
        self.request_code = _NonStreamRequest._call.__code__
        if Path(self.request_code.co_filename).resolve() != (source / "agent" / "chat_completion_nonstream.py").resolve():
            raise Refused("request_lifecycle_unavailable")
        from tools.browser_harness_dirs import fit_default_socket_dir
        # The native CLI and read-only probes must address the same named daemon.
        # These public helpers capture their directories at import time.
        os.environ["BU_NAME"] = self.session_name
        fit_default_socket_dir(os.environ)
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

    def mark_lost(self):
        self.lost = True
        self.halt("uncertain")

    def browser_identity(self):
        """Read only existing Hermes/daemon state; never provision a resource."""
        import psutil
        from hermes_cli.process_identity import spawned_by_this_process
        from tools.browser_harness_dirs import harness_pid_file
        from tools.browser_lightpanda import get_server
        from tools.browser_session import session_for_task

        cached = session_for_task("bu-named-" + self.session_name)
        if not (cached.get("features") or {}).get("lightpanda"):
            raise Refused("browser_identity_unavailable")
        server = get_server(cached.get("session_name", ""))
        if server is None or not server.is_alive() or cached.get("cdp_url") != server.cdp_url:
            raise Refused("browser_identity_unavailable")
        native = psutil.Process(server.proc.pid)
        if native.ppid() != os.getpid() or native.uids().real != os.getuid():
            raise Refused("browser_identity_unavailable")
        pid_file = harness_pid_file(os.environ)
        if pid_file.stat().st_size > 4096:
            raise Refused("daemon_identity_unavailable")
        receipt = json.loads(pid_file.read_text())
        if type(receipt) is int:
            pid, token = receipt, None
        elif isinstance(receipt, dict):
            pid, token = receipt.get("pid"), receipt.get("started")
        else:
            raise Refused("daemon_identity_unavailable")
        if (type(pid) is not int or pid <= 1 or
                token is not None and (not isinstance(token, str) or not token or len(token) > 128)):
            raise Refused("daemon_identity_unavailable")
        daemon = psutil.Process(pid)
        arguments = daemon.cmdline()
        module = arguments[arguments.index("-m") + 1] if "-m" in arguments else ""
        if (daemon.uids().real != os.getuid() or os.getsid(pid) != pid or
                module != "browser_harness.daemon" or not spawned_by_this_process(pid)):
            raise Refused("daemon_identity_unavailable")
        return {"daemon_pid": pid, "daemon_birth": daemon.create_time(), "daemon_token": token,
                "native_pid": native.pid, "native_birth": native.create_time(),
                "endpoint": server.cdp_url}

    def pin_browser(self):
        from browser_harness.admin import daemon_browser_ready
        from browser_harness.helpers import cdp, current_tab

        identity = self.browser_identity()
        if not daemon_browser_ready(self.session_name):
            raise Refused("browser_connection_unavailable")
        target = current_tab().get("targetId")
        if not isinstance(target, str) or not target:
            raise Refused("browser_connection_unavailable")
        session = cdp("Target.attachToTarget", targetId=target, flatten=True,
                      _response_timeout=1.0).get("sessionId")
        if not isinstance(session, str) or not session:
            raise Refused("browser_connection_unavailable")
        if self.browser_identity() != identity:
            raise Refused("browser_identity_changed")
        self.browser_pin = {**identity, "target": target, "session": session}
        # This documented override short-circuits get_session_info on subsequent
        # native calls. Together with require-existing it prevents provisioning a
        # replacement Lightpanda process or harness daemon for this session.
        os.environ["BU_CDP_URL"] = identity["endpoint"]
        os.environ["BH_REQUIRE_EXISTING_DAEMON"] = "1"
        if self.probe_browser() != "connected":
            raise Refused("browser_connection_unavailable")

    def probe_browser(self):
        """Called under native_lock: use the pinned connection without recovery."""
        if self.lost:
            return "lost"
        if self.browser_pin is None:
            if self.browser_attempted:
                self.mark_lost()
                return "lost"
            return "starting"
        try:
            from browser_harness.admin import daemon_browser_ready
            from browser_harness.helpers import cdp, current_tab
            from tools.browser_session_lifecycle import update_session_activity

            pin = self.browser_pin
            identity = self.browser_identity()
            if any(identity[name] != pin[name] for name in identity) or not daemon_browser_ready(self.session_name):
                raise Refused("browser_identity_changed")
            if current_tab().get("targetId") != pin["target"]:
                raise Refused("browser_target_changed")
            reply = cdp("Runtime.evaluate", session_id=pin["session"], expression="true",
                        returnByValue=True, awaitPromise=False, _response_timeout=1.0)
            if reply.get("exceptionDetails") or reply.get("result", {}).get("value") is not True:
                raise Refused("browser_connection_unavailable")
            if self.browser_identity() != identity:
                raise Refused("browser_identity_changed")
            update_session_activity("bu-named-" + self.session_name)
            return "connected"
        except Exception:
            self.mark_lost()
            return "lost"

    def health(self):
        if self.lost:
            return "lost"
        if not self.native_lock.acquire(blocking=False):
            return "unverified"
        try:
            return self.probe_browser()
        finally:
            self.native_lock.release()

    def request_workers(self, agent):
        """Observe only this agent's pinned request frames; never alter them."""
        if getattr(agent, "api_mode", None) != "chat_completions":
            raise Refused("request_lifecycle_unavailable")
        threads = {thread.ident: thread for thread in threading.enumerate()}
        found = set()
        for identifier, frame in sys._current_frames().items():
            while frame is not None:
                if frame.f_code.co_filename == self.request_code.co_filename and frame.f_code.co_name == "_call":
                    owner = frame.f_locals.get("self")
                    if getattr(owner, "agent", None) is agent:
                        thread = threads.get(identifier)
                        if type(owner) is not self.request_type or frame.f_code is not self.request_code or thread is None:
                            raise Refused("request_lifecycle_unavailable")
                        found.add(thread)
                frame = frame.f_back
        return found

    def settle_request_workers(self, run, agent):
        try:
            deadline = time.monotonic() + 2
            while True:
                with self.lock:
                    run.request_threads.update(self.request_workers(agent))
                    threads = set(run.request_threads)
                for thread in threads:
                    thread.join(timeout=max(0, deadline - time.monotonic()))
                if not any(thread.is_alive() for thread in threads) and not self.request_workers(agent):
                    return True
                if time.monotonic() >= deadline:
                    return False
                time.sleep(0.02)
        except Exception:
            return False

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

    def event(self, run, payload, *, wait=True, kind="event"):
        identifier = run.id + ":" + secrets.token_hex(12)
        waiter = queue.Queue(maxsize=1)
        with self.lock:
            if (run.cancelled.is_set() or run.finished.is_set() or self.closing or
                    self.tainted or self.lost or self.active is not run):
                raise Refused("execution_cancelled")
            if wait:
                self.pending[identifier] = waiter
        try:
            with self.lock:
                if run.finished.is_set() or self.tainted:
                    raise Refused("execution_cancelled")
                self.send(kind, identifier, payload)
            if not wait:
                return {}
            while not (run.cancelled.is_set() or run.finished.is_set() or
                       self.closing or self.tainted or self.lost):
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

    def halt(self, status, *, owner=None):
        with self.lock:
            run = self.active
            if run is None or owner is not None and run is not owner:
                return
            run.halted = run.halted or status
            agent = run.agent
        if agent is not None:
            try:
                agent.interrupt(hard_cancel=True, tool_reason="browser execution stopped")
            except Exception:
                pass

    def deny_terminal(self):
        run = self.execution.get()
        if run is not None:
            self.halt("failed", owner=run)

    def browser(self, handler, args):
        run = self.execution.get()
        with self.native_lock:
            with self.lock:
                current = self.active is run
            if (run is None or not current or run.finished.is_set() or run.halted or run.cancelled.is_set() or
                    self.closing or self.tainted or self.lost):
                return json.dumps({"error": "Browser execution is unavailable."})
            try:
                if self.probe_browser() == "lost":
                    raise Refused("browser_session_lost")
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
                self.halt("failed", owner=run)
                return json.dumps({"error": "Browser admission was refused."})
            run.in_native += 1
            self.browser_attempted = True
            try:
                # The public registry supplied this exact native handler. Do not
                # redispatch the overridden name or forward model session routing.
                result = handler({"code": code, "session": "" if self.browser_pin else self.session_name,
                                  "timeout_s": min(remaining, 300)}, task_id=self.session_name)
                if run.cancelled.is_set():
                    raise Refused("execution_cancelled")
                encoded = result if isinstance(result, str) else json.dumps(result)
                encoded = run.redact(encoded)
                if len(encoded.encode("utf-8")) > MAX_NATIVE_RESULT:
                    raise Refused("browser_output_limit")
                # The Go owner holds non-plaintext fingerprints for submitted
                # values, including old keys. Protect observations before
                # Hermes can record or replay them, without delivering those
                # credentials to this process.
                protected = self.event(run, {"sequence": run.sequence, "text": encoded}, kind="protect")
                encoded = bounded_text(protected.get("text"), MAX_NATIVE_RESULT)
                parsed = json.loads(encoded)
                if isinstance(parsed, dict) and (parsed.get("success") is False or "error" in parsed):
                    raise Refused("browser_operation_unconfirmed")
                if self.browser_pin is None:
                    self.pin_browser()
                elif self.probe_browser() != "connected":
                    raise Refused("browser_session_lost")
                self.event(run, {"kind": "progress", "sequence": run.sequence,
                                 "tool": "browser_exec", "text": "finished"}, wait=False)
                return encoded
            except Exception:
                if self.browser_pin is None or self.probe_browser() == "lost":
                    self.mark_lost()
                self.halt("uncertain", owner=run)
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
            self.halt("failed", owner=run)
            return {"answers": {}, "outcome": "cancelled"}

    def execute(self, run):
        execution_token = self.execution.set(run)
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
            count = raw.get("api_calls", 0)
            if type(count) is not int or not 0 <= count <= 1000000:
                raise Refused("invalid_usage")
            status = run.halted or ("interrupted" if run.cancelled.is_set() or raw.get("interrupted") else
                                    "failed" if raw.get("failed") else "completed" if raw.get("completed") else "incomplete")
            text = run.redact(raw.get("final_response") or "")
            text = text.encode("utf-8", "replace")[:MAX_TEXT].decode("utf-8", "ignore")
            result = {"status": status, "text": text, "model_requests": count}
        except Exception:
            # An unexpected escape has no public receipt that Hermes joined all
            # tool callbacks. Never let a delayed callback adopt a later run.
            self.tainted = True
            run.halted = "uncertain"
            result = {"status": "uncertain"}
        finally:
            if agent is not None:
                try:
                    agent.release_clients()
                except Exception:
                    self.tainted = True
                    result = {"status": "uncertain"}
                if not self.settle_request_workers(run, agent):
                    self.tainted = True
                    result = {"status": "uncertain"}
            native_quiet = self.native_lock.acquire(blocking=False)
            try:
                with self.lock:
                    if (not native_quiet or run.in_native or self.tainted or
                            any(identifier.startswith(run.id + ":") for identifier in self.pending)):
                        self.tainted = True
                        run.halted = "uncertain"
                        result = {"status": "uncertain"}
                    run.agent = None
                    run.key = ""
                    run.finished.set()
                    if self.active is run:
                        self.active = None
                    if native_quiet:
                        self.native_lock.release()
                        native_quiet = False
                    # A receipt also hands control back to independent health
                    # checks. Revoke old callbacks before releasing native work,
                    # then publish only after that gate is free.
                    self.send("result", run.id, result)
            finally:
                if native_quiet:
                    self.native_lock.release()
                self.execution.reset(execution_token)

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
                threads = self.request_workers(agent)
                with self.lock:
                    run.request_threads.update(threads)
            except Exception:
                self.tainted = True
                run.halted = "uncertain"
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
                        root = psutil.Process(entry["pid"])
                        processes.append(root)
                        # The harness daemon detaches from this worker. Its
                        # invocation children must be remembered before their
                        # daemon exits and they are reparented to init.
                        processes.extend(root.children(recursive=True))
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
                self.tainted = True
        self.remember_owned()
        try:
            import psutil
            from hermes_cli.process_identity import is_owner_death_supervisor

            def signal_owned(*, kill):
                with self.lock:
                    owned = dict(self.owned)
                signalled = False
                for pid, birth in owned.items():
                    try:
                        process = psutil.Process(pid)
                        if (process.create_time() != birth or process.uids().real != os.getuid() or
                                self.supervisors.get(pid) == birth and process.ppid() == os.getpid() and
                                is_owner_death_supervisor(process.cmdline())):
                            continue
                        self.tainted = True  # Forced shutdown is never a clean receipt.
                        process.kill() if kill else process.terminate()
                        signalled = True
                    except (psutil.NoSuchProcess, ProcessLookupError):
                        pass
                return signalled

            if self.tainted or self.lost:
                # An abandoned native call can block daemon shutdown. Capture
                # its detached descendants first, then stop exact owned PIDs
                # before asking the public daemon cleanup to join.
                if signal_owned(kill=False):
                    time.sleep(0.5)
                    self.remember_owned()
                    signal_owned(kill=True)
            from tools.browser_session_lifecycle import cleanup_all_browsers
            from tools.browser_use_cli import stop_harness_daemons
            try:
                cleanup_all_browsers()
            except Exception:
                self.tainted = True
            try:
                stop_harness_daemons()
            except Exception:
                self.tainted = True
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
                if self.tainted or self.lost:
                    # Public cleanup can leave an abandoned native invocation
                    # running below a detached daemon. Stop only exact recorded
                    # PIDs with their original birth identity, never a bare PGID.
                    signal_owned(kill=True)
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
        if kind == "verify":
            if payload:
                raise Refused("invalid_frame")
            self.send("health", identifier, {"state": self.health()})
            return True
        if kind != "run":
            raise Refused("invalid_frame")
        with self.lock:
            if self.active is not None or self.closing or self.tainted or self.lost:
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
