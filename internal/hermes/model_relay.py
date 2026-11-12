"""Execution-scoped loopback HTTP over private model frames, never provider I/O.

Container requests and replies are each limited to 512 KiB of raw bytes. Base64
keeps each correlated message below the bridge's 1 MiB frame bound. The local
process backend keeps its existing, separate transport limits.
"""

import base64
import hmac
from http.server import BaseHTTPRequestHandler, HTTPServer
import queue
import secrets
import socket
import socketserver
import threading
import time


MAX_BODY = 512 << 10
MAX_ENCODED = ((MAX_BODY + 2) // 3) * 4
FAILURE = b'{"error":{"message":"Model request unavailable","type":"invalid_request_error"}}'


class RelayUnavailable(Exception):
    pass


class ModelRelay:
    def __init__(self, worker, run):
        self.worker, self.run = worker, run
        self.lock = threading.Lock()
        self.stopped = threading.Event()
        self.server = self.thread = self.connection = None
        self.pending = None
        self.fault = False
        self.base_url = ""

    def start(self):
        with self.lock:
            if self.stopped.is_set() or self.run.cancelled.is_set():
                raise RelayUnavailable("relay_unavailable")
            self.server = _Server(self)
            self.base_url = "http://127.0.0.1:%d/v1" % self.server.server_address[1]
            self.thread = threading.Thread(target=self.serve, name="commander-model-relay", daemon=True)
            self.thread.start()

    def serve(self):
        try:
            while not self.stopped.is_set():
                self.server.handle_request()
        except Exception:
            if not self.stopped.is_set():
                self.mark_fault()
        finally:
            self.server.server_close()

    def mark_fault(self):
        with self.lock:
            self.fault = True

    def exchange(self, method, path, body):
        identifier = self.run.id + ":model:" + secrets.token_hex(12)
        waiter = queue.Queue(maxsize=1)
        with self.lock:
            if self.stopped.is_set() or self.pending is not None:
                raise RelayUnavailable("relay_unavailable")
            self.pending = (identifier, waiter)
        try:
            with self.worker.lock:
                if (self.worker.active is not self.run or self.worker.closing or
                        self.worker.tainted or self.worker.lost or self.run.cancelled.is_set() or
                        self.run.finished.is_set() or time.monotonic() >= self.run.deadline):
                    raise RelayUnavailable("relay_unavailable")
            # Keep the reader/control lock free while a bounded frame writes.
            # Go binds even a late frame to this run's original cancelled context.
            self.worker.send("model", identifier, {"method": method, "path": path,
                             "token": self.run.key, "body": base64.b64encode(body).decode("ascii")})
            while not (self.stopped.is_set() or self.run.cancelled.is_set() or self.run.finished.is_set()):
                remaining = self.run.deadline - time.monotonic()
                if remaining <= 0:
                    break
                try:
                    response = waiter.get(timeout=min(remaining, .1))
                except queue.Empty:
                    continue
                if response is not None:
                    return response
                break
            raise RelayUnavailable("relay_unavailable")
        finally:
            with self.lock:
                if self.pending is not None and self.pending[0] == identifier:
                    self.pending = None

    def reply(self, identifier, payload):
        with self.lock:
            # An abandoned call belongs only to its original run. Late replies
            # cannot acquire the next run's waiter or model authority.
            if self.stopped.is_set() or self.pending is None or self.pending[0] != identifier:
                return True
            valid = (isinstance(payload, dict) and set(payload) == {"status", "body"} and
                     type(payload["status"]) is int and payload["status"] in (200, 400) and
                     isinstance(payload["body"], str) and len(payload["body"]) <= MAX_ENCODED)
            try:
                if not valid:
                    raise ValueError()
                body = base64.b64decode(payload["body"], validate=True)
                if len(body) > MAX_BODY:
                    raise ValueError()
                self.pending[1].put_nowait((payload["status"], body))
            except (ValueError, queue.Full):
                self.fault = True
                return False
            return True

    def cancel(self):
        self.stopped.set()
        with self.lock:
            pending, connection, server = self.pending, self.connection, self.server
            if pending is not None:
                try:
                    pending[1].put_nowait(None)
                except queue.Full:
                    pass
        if server is not None:
            server.server_close()
        # Let a pending host exchange return a non-retryable HTTP error first.
        # Header/body readers have no waiter and must be interrupted directly.
        if pending is None:
            self.shutdown(connection)

    @staticmethod
    def shutdown(connection):
        if connection is not None:
            try:
                connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            try:
                connection.close()
            except OSError:
                pass

    def close(self):
        self.cancel()
        with self.lock:
            thread = self.thread
        if thread is not None:
            thread.join(1)
            if thread.is_alive():
                with self.lock:
                    connection = self.connection
                self.shutdown(connection)
                thread.join(1)
        with self.lock:
            return (not self.fault and self.pending is None and self.connection is None and
                    (thread is None or not thread.is_alive()))

    def quiet(self):
        with self.lock:
            return (self.stopped.is_set() and self.pending is None and self.connection is None and
                    (self.thread is None or not self.thread.is_alive()) and not self.fault)


class _Server(HTTPServer):
    request_queue_size = 1

    def __init__(self, relay):
        self.relay = relay
        super().__init__(("127.0.0.1", 0), _Handler)
        self.timeout = .1

    def server_bind(self):
        # The endpoint is always loopback; no reverse-DNS metadata probe is needed.
        socketserver.TCPServer.server_bind(self)
        self.server_name, self.server_port = "localhost", self.server_address[1]

    def get_request(self):
        connection, address = super().get_request()
        connection.settimeout(max(.01, min(5, self.relay.run.deadline - time.monotonic())))
        with self.relay.lock:
            if self.relay.stopped.is_set():
                connection.close()
                raise OSError("relay_unavailable")
            self.relay.connection = connection
        return connection, address

    def shutdown_request(self, request):
        try:
            super().shutdown_request(request)
        finally:
            with self.relay.lock:
                if self.relay.connection is request:
                    self.relay.connection = None

    def handle_error(self, _request, _address):
        if not self.relay.stopped.is_set():
            self.relay.mark_fault()


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "CommanderModelRelay"
    sys_version = ""

    def log_message(self, _format, *_arguments):
        pass

    def handle_expect_100(self):
        self.respond(400, FAILURE)
        return False

    def send_error(self, _code, _message=None, _explain=None):
        self.respond(400, FAILURE)

    def respond(self, status, body):
        self.close_connection = True
        try:
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(body)
        except OSError:
            pass

    def do_GET(self):
        self.forward()

    def do_POST(self):
        self.forward()

    def forward(self):
        relay = self.server.relay
        try:
            authorization = self.headers.get_all("Authorization", [])
            lengths = self.headers.get_all("Content-Length", [])
            valid = (not relay.stopped.is_set() and len(authorization) == 1 and
                     hmac.compare_digest(authorization[0].encode(), ("Bearer " + relay.run.key).encode()) and
                     self.headers.get("Transfer-Encoding") is None and
                     sum(len(name) + len(value) + 4 for name, value in self.headers.items()) <= 16384)
            if not valid or self.command == "GET" and self.path != "/v1/models" or self.command == "POST" and self.path != "/v1/chat/completions":
                raise RelayUnavailable("relay_unavailable")
            if len(lengths) > 1 or self.command == "POST" and len(lengths) != 1:
                raise RelayUnavailable("relay_unavailable")
            raw_length = lengths[0] if lengths else "0"
            if not raw_length.isascii() or not raw_length.isdecimal() or len(raw_length) > 7:
                raise RelayUnavailable("relay_unavailable")
            length = int(raw_length)
            if length > MAX_BODY or self.command == "GET" and length != 0:
                raise RelayUnavailable("relay_unavailable")
            body = self.rfile.read(length)
            if len(body) != length:
                raise RelayUnavailable("relay_unavailable")
            status, output = relay.exchange(self.command, self.path, body)
            self.respond(status, output)
        except Exception:
            self.respond(400, FAILURE)
