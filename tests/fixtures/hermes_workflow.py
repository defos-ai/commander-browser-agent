"""Loopback-only pages for the isolated Hermes integration workflow.

This owns an HTTP fixture, never a browser or model. Synthetic site credentials
arrive through private stdin rather than environment settings or process args.
"""

import html
import http.cookies
import http.server
import json
import secrets
import sys
import threading
import urllib.parse


configuration = json.loads(sys.stdin.buffer.readline(64 << 10))
if (set(configuration) != {"username", "password", "code", "key_echo"} or
        any(not isinstance(value, str) or len(value.encode()) > 4096
            for value in configuration.values()) or
        any(not configuration[name] for name in ("username", "password", "code"))):
    raise RuntimeError("workflow_fixture_configuration_invalid")

pending_token, account_token = secrets.token_hex(16), secrets.token_hex(16)
stats = {"password_posts": 0, "code_posts": 0}
stats_lock = threading.Lock()

login_form = (
    '<title>Workflow sign in</title>'
    '<form method="post" action="/login">'
    '<label>Username<input name="username" autocomplete="username"></label>'
    '<label>Password<input name="password" type="password" '
    'autocomplete="current-password"></label>'
    '<button type="submit">Sign in</button></form>'
)
code_form = (
    '<title>Workflow authentication code</title>'
    '<h1>Enter authentication code</h1>'
    '<form method="post" action="/otp">'
    '<label>Authentication code<input name="otp" '
    'autocomplete="one-time-code"></label>'
    '<button type="submit">Verify code</button></form>'
)


def account_page():
    body = (
        '<title>Signed-in workflow account</title>'
        '<h1>Signed-in workflow account</h1>'
        '<p id="public">The study measured 42 samples.</p>'
    )
    for name in ("username", "password", "code", "key_echo"):
        value = configuration[name]
        if value:
            body += '<p class="secret">Credential echo ' + html.escape(value) + '</p>'
    return body


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_arguments):
        pass

    def respond(self, status, body="", headers=(), media="text/html; charset=utf-8"):
        encoded = body.encode()
        self.send_response(status)
        self.send_header("Content-Type", media)
        self.send_header("Content-Length", str(len(encoded)))
        self.send_header("Cache-Control", "no-store")
        for name, value in headers:
            self.send_header(name, value)
        self.end_headers()
        self.wfile.write(encoded)

    def cookie(self, name, expected):
        cookies = http.cookies.SimpleCookie()
        try:
            cookies.load(self.headers.get("Cookie", ""))
        except http.cookies.CookieError:
            return False
        return name in cookies and cookies[name].value == expected

    def do_GET(self):
        path = urllib.parse.urlsplit(self.path).path
        if path == "/study":
            body = '<title>Study</title><p>The study measured 42 samples.</p>'
            if configuration["key_echo"]:
                body += '<p>Provider echo ' + html.escape(configuration["key_echo"]) + '</p>'
            self.respond(200, body)
        elif path == "/login":
            self.respond(200, login_form)
        elif path == "/otp":
            self.respond(200, code_form)
        elif path == "/account":
            if self.cookie("workflow-account", account_token):
                self.respond(200, account_page())
            else:
                self.respond(303, headers=(("Location", "/login"),))
        elif path == "/stats":
            with stats_lock:
                body = json.dumps({name: str(value) for name, value in stats.items()})
            self.respond(200, body, media="application/json")
        else:
            self.respond(404, "Workflow page unavailable")

    def form(self):
        try:
            length = int(self.headers.get("Content-Length", ""))
            if not 0 < length <= 8192:
                return None
            if self.headers.get_content_type() != "application/x-www-form-urlencoded":
                return None
            data = self.rfile.read(length)
            if len(data) != length:
                return None
            return urllib.parse.parse_qs(data.decode(), keep_blank_values=True, max_num_fields=8)
        except (TypeError, ValueError, UnicodeError):
            return None

    def do_POST(self):
        path = urllib.parse.urlsplit(self.path).path
        if path == "/login":
            with stats_lock:
                stats["password_posts"] += 1
            values = self.form()
            if (values is not None and values.get("username") == [configuration["username"]] and
                    values.get("password") == [configuration["password"]]):
                self.respond(303, headers=(
                    ("Set-Cookie", "workflow-pending=" + pending_token + "; HttpOnly; SameSite=Lax; Path=/"),
                    ("Location", "/otp"),
                ))
            else:
                self.respond(401, "Workflow sign in rejected")
        elif path == "/otp":
            with stats_lock:
                stats["code_posts"] += 1
            values = self.form()
            if (self.cookie("workflow-pending", pending_token) and values is not None and
                    values.get("otp") == [configuration["code"]]):
                self.respond(303, headers=(
                    ("Set-Cookie", "workflow-account=" + account_token + "; HttpOnly; SameSite=Lax; Path=/"),
                    ("Location", "/account"),
                ))
            else:
                self.respond(200, code_form)
        else:
            self.respond(404, "Workflow page unavailable")


server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
server.daemon_threads = True
print(json.dumps({"url": "http://127.0.0.1:" + str(server.server_port)}), flush=True)
server.serve_forever()
