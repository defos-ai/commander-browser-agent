"""Host-only CDP leases over the original browser-harness daemon connection.

No model tool, Python evaluation, new WebSocket or upstream registration is used
for these commands. Credentials may occur only in private CDP argument values.
"""

import json
import re
import threading
import time


MAX_PARAMS = 32 << 10
MAX_RESULT = 128 << 10
MAX_EVENT_RESULT = 32 << 10
MAX_EVENTS = 128  # The pinned daemon silently evicts at 500 total events.
METHODS = frozenset({
    "Page.enable", "Page.navigate", "Page.getFrameTree", "Page.createIsolatedWorld", "Page.stopLoading",
    "Runtime.enable", "Runtime.evaluate", "Runtime.callFunctionOn", "Network.enable",
    "Fetch.enable", "Fetch.disable", "Fetch.continueRequest", "Fetch.failRequest",
})
EVENTS = frozenset({"Fetch.requestPaused", "Network.requestWillBeSent",
                    "Network.responseReceived", "Network.loadingFinished", "Network.loadingFailed"})


class Lease:
    def __init__(self, identifier):
        self.id = identifier
        self.ready = False
        self.closing = False
        self.failed = False
        self.locked = False
        self.threads = set()


class NativeBrowser:
    def __init__(self, worker):
        self.worker = worker
        self.lock = threading.RLock()
        self.events_lock = threading.Lock()
        self.current = None
        self.threads = set()
        self.closed = False

    def busy(self):
        with self.lock:
            return self.current is not None

    def reply(self, identifier, *, ok, result=None, state=None):
        if self.worker.closing:
            return
        payload = {"ok": ok}
        if result is not None:
            payload["result"] = result
        if state is not None:
            payload["state"] = state
        if len(json.dumps(payload, ensure_ascii=True).encode()) > MAX_RESULT + 256:
            raise RuntimeError("browser_result_limit")
        self.worker.send("browser", identifier, payload)

    def fail(self, lease):
        with self.lock:
            lease.failed = True
        with self.worker.lock:
            if not self.worker.closing:
                self.worker.mark_lost()

    def identity(self, *, allow_lost=False):
        pin = self.worker.browser_pin
        if pin is None or self.worker.lost and not allow_lost or self.worker.closing:
            raise RuntimeError("browser_unavailable")
        identity = self.worker.browser_identity()
        route = {name: pin[name] for name in ("target", "automation_session")}
        if (any(pin.get(name) != value for name, value in identity.items()) or
                self.worker.browser_route() != route):
            raise RuntimeError("browser_unavailable")
        return pin

    def start(self, lease, identifier, operation, *, control=False):
        def run():
            try:
                result, state = operation()
                self.reply(identifier, ok=True, result=result, state=state)
            except Exception:
                self.fail(lease)
                self.reply(identifier, ok=False, state="lost")
            finally:
                with self.lock:
                    self.threads.discard(threading.current_thread())
                    lease.threads.discard(threading.current_thread())

        with self.lock:
            if self.closed or not control and len(self.threads) >= 8:
                return False
            thread = threading.Thread(target=run, name="commander-private-browser", daemon=True)
            self.threads.add(thread)
            lease.threads.add(thread)
            thread.start()
            return True

    def accept(self, identifier, payload):
        lease_id, command = payload.get("lease"), payload.get("command")
        method, params = payload.get("method"), payload.pop("params", None)
        if command == "call" and params is None:
            params = {}
        timeout_ms, uncertain = payload.get("timeout_ms"), payload.get("uncertain", False)
        require_connected = payload.get("require_connected", False)
        keys = set(payload)
        payload.clear()  # The reader must not retain a second credential map.
        valid = (isinstance(lease_id, str) and re.fullmatch(r"[a-f0-9]{32}", lease_id) and
                 identifier.startswith(lease_id + ":") and len(identifier) > len(lease_id) + 1 and
                 type(timeout_ms) is int and 1 <= timeout_ms <= 8000 and type(uncertain) is bool and
                 type(require_connected) is bool)
        if not valid or keys - {"lease", "command", "method", "timeout_ms", "uncertain", "require_connected"}:
            if isinstance(params, dict):
                params.clear()
            self.reply(identifier, ok=False, state="lost")
            return
        if command == "begin":
            with self.worker.lock:
                with self.lock:
                    if (self.closed or self.current is not None or self.worker.active is not None or
                            self.worker.closing or self.worker.tainted or self.worker.lost or method is not None or params is not None):
                        if isinstance(params, dict):
                            params.clear()
                        self.reply(identifier, ok=False, state="lost")
                        return
                    lease = self.current = Lease(lease_id)
            if not self.start(lease, identifier, lambda: self.begin(lease, timeout_ms, require_connected)):
                self.fail(lease)
                self.reply(identifier, ok=False, state="lost")
            return
        with self.lock:
            lease = self.current
            valid = (lease is not None and lease.id == lease_id and not lease.closing and
                     (command == "end" or lease.ready and (not lease.failed or command == "destroy")))
            if valid and command == "end":
                lease.closing = True
        if not valid:
            if isinstance(params, dict):
                params.clear()
            self.reply(identifier, ok=False, state="lost")
            return
        if command == "end" and method is None and params is None:
            operation = lambda: self.end(lease, uncertain)
        elif command == "events" and method is None and params is None:
            operation = lambda: (self.events(), None)
        elif command == "destroy" and method is None and params is None:
            operation = lambda: self.destroy(lease)
        elif command == "renderer" and method is None and params is None:
            operation = lambda: ({"endpoint": self.identity()["endpoint"]}, None)
        elif (command == "call" and isinstance(method, str) and method in METHODS and isinstance(params, dict) and
              not set(params).intersection({"session_id", "sessionId", "target_id", "targetId"})):
            operation = lambda: (self.call(method, params, timeout_ms), None)
        else:
            if isinstance(params, dict):
                params.clear()
            self.fail(lease)
            self.reply(identifier, ok=False, state="lost")
            return
        if not self.start(lease, identifier, operation, control=command == "end"):
            if isinstance(params, dict):
                params.clear()
            self.fail(lease)
            self.reply(identifier, ok=False, state="lost")

    def begin(self, lease, timeout_ms, require_connected):
        if not self.worker.native_lock.acquire(timeout=min(2, timeout_ms / 1000)):
            raise RuntimeError("browser_unavailable")
        lease.locked = True
        if self.worker.closing or lease.closing:
            raise RuntimeError("browser_unavailable")
        state = self.worker.probe_browser()
        if state == "starting":
            if require_connected:
                raise RuntimeError("browser_not_connected")
            from tools.browser_use_cli import browser_exec
            self.worker.browser_attempted = True
            result = browser_exec("print('ready')", session=self.worker.session_name,
                                  task_id=self.worker.session_name, timeout_s=5)
            result = json.loads(result) if isinstance(result, str) else result
            if not isinstance(result, dict) or result.get("success") is not True:
                raise RuntimeError("browser_unavailable")
            self.worker.pin_browser()
        elif state != "connected":
            raise RuntimeError("browser_unavailable")
        if self.worker.closing or lease.closing:
            raise RuntimeError("browser_unavailable")
        with self.lock:
            lease.ready = True
        return None, "connected"

    def call(self, method, params, timeout_ms):
        try:
            if len(json.dumps(params, ensure_ascii=True).encode()) > MAX_PARAMS:
                raise RuntimeError("browser_request_limit")
            pin = self.identity()
            from browser_harness.helpers import cdp
            result = cdp(method, session_id=pin["automation_session"],
                         _response_timeout=min(8, timeout_ms / 1000), **params)
            if result.get("exceptionDetails") or len(json.dumps(result, ensure_ascii=True).encode()) > MAX_RESULT:
                raise RuntimeError("browser_result_unconfirmed")
            self.identity()
            return result
        finally:
            params.clear()

    @staticmethod
    def text(value, maximum):
        if not isinstance(value, str) or not value or len(value.encode()) > maximum or "\x00" in value:
            raise RuntimeError("browser_event_unavailable")
        return value

    def events(self):
        if not self.events_lock.acquire(blocking=False):
            raise RuntimeError("browser_event_unavailable")
        try:
            pin = self.identity()
            from browser_harness.helpers import drain_events
            events = drain_events()
            if not isinstance(events, list) or len(events) >= MAX_EVENTS:
                raise RuntimeError("browser_event_limit")
            output = []
            for event in events:
                if not isinstance(event, dict):
                    raise RuntimeError("browser_event_unavailable")
                method = event.get("method")
                if event.get("session_id") != pin["automation_session"] or method not in EVENTS:
                    continue
                params = event.get("params") or {}
                selected = {"requestId": self.text(params.get("requestId"), 256)}
                if "frameId" in params:
                    selected["frameId"] = self.text(params["frameId"], 256)
                if method in {"Fetch.requestPaused", "Network.requestWillBeSent"}:
                    request = params.get("request") or {}
                    selected["request"] = {"url": self.text(request.get("url"), 4096),
                                           "method": self.text(request.get("method"), 16)}
                resource_key = "resourceType" if method == "Fetch.requestPaused" else "type"
                if method == "Fetch.requestPaused" or resource_key in params:
                    selected[resource_key] = self.text(params.get(resource_key), 64)
                response_key = ("response" if method == "Network.responseReceived" else
                                "redirectResponse" if method == "Network.requestWillBeSent" else None)
                if response_key is not None and response_key in params:
                    response = params[response_key]
                    status = response.get("status") if isinstance(response, dict) else None
                    if type(status) is not int or not 100 <= status <= 599:
                        raise RuntimeError("browser_event_unavailable")
                    selected[response_key] = {"status": status}
                output.append({"method": method, "params": selected})
            if len(json.dumps(output, ensure_ascii=True).encode()) > MAX_EVENT_RESULT:
                raise RuntimeError("browser_event_limit")
            self.identity()
            return output
        finally:
            self.events_lock.release()

    def destroy(self, lease):
        pin = self.identity(allow_lost=True)
        from browser_harness.helpers import cdp
        result = cdp("Target.closeTarget", targetId=pin["target"], _response_timeout=2.0)
        self.fail(lease)
        return result, "lost"

    def join(self, threads, seconds=2):
        deadline = time.monotonic() + seconds
        for thread in threads:
            if thread is not threading.current_thread():
                thread.join(max(0, deadline - time.monotonic()))
        return all(thread is threading.current_thread() or not thread.is_alive() for thread in threads)

    def release(self, lease):
        with self.lock:
            if lease.locked:
                lease.locked = False
                self.worker.native_lock.release()
            if self.current is lease:
                self.current = None

    def end(self, lease, uncertain):
        try:
            with self.lock:
                threads = set(lease.threads)
            if not self.join(threads) or uncertain or lease.failed or self.worker.probe_browser() != "connected":
                raise RuntimeError("browser_unavailable")
            return None, "connected"
        except Exception:
            self.fail(lease)
            raise
        finally:
            self.release(lease)

    def close(self):
        with self.lock:
            self.closed = True
            lease = self.current
            if lease is not None:
                lease.closing = True
            threads = set(self.threads)
        joined = self.join(threads)
        if lease is not None:
            if not joined:
                self.fail(lease)
            self.release(lease)
        return joined
