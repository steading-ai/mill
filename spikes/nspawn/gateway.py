#!/usr/bin/env python3
"""The credential gateway of docs/sandbox.md §Credentials, as a probe.

A session inside a machine holds a step token in ANTHROPIC_AUTH_TOKEN and
points ANTHROPIC_BASE_URL here. This proxy swaps the bearer for the
subscription token it holds in its own environment and forwards the request
to the upstream unchanged otherwise, streaming the response back. It logs the
inbound token and the outbound host per request; the outbound token is never
logged. Only paths under /v1/messages are forwarded; everything else is 404.

Environment:
  CLAUDE_CODE_OAUTH_TOKEN  the subscription token (a `claude setup-token`)
  GATEWAY_BIND             host:port to listen on (default 127.0.0.1:8080)
  GATEWAY_UPSTREAM         upstream origin (default https://api.anthropic.com)
  GATEWAY_LOG              log file (default stderr)
  GATEWAY_SWAP             "0" leaves the Authorization header untouched (the
                           fallback of §Credentials); default "1"
  GATEWAY_DUMP             a directory; when set, every forwarded request is
                           written there as <n>.json (the body; the tool-search
                           probe reads its `tools` array) and <n>.headers (the
                           inbound headers, Authorization omitted)

The probe is standard library only and stays under two hundred lines; mill#57
builds the real thing in Go.
"""

import http.client
import os
import ssl
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

UPSTREAM = urlsplit(os.environ.get("GATEWAY_UPSTREAM", "https://api.anthropic.com"))
SWAP = os.environ.get("GATEWAY_SWAP", "1") != "0"
SUBSCRIPTION_TOKEN = os.environ.get("CLAUDE_CODE_OAUTH_TOKEN", "")
DUMP = os.environ.get("GATEWAY_DUMP")

# Headers that describe the connection, not the request; the proxy owns those.
HOP_BY_HOP = {"connection", "keep-alive", "proxy-connection", "transfer-encoding", "te", "upgrade", "host"}

_log = open(os.environ["GATEWAY_LOG"], "a", buffering=1) if os.environ.get("GATEWAY_LOG") else sys.stderr


def log(msg):
    _log.write(msg + "\n")
    _log.flush()


def bearer(value):
    """The token in an `Authorization: Bearer <token>` value, or ''."""
    scheme, _, token = (value or "").partition(" ")
    return token.strip() if scheme.lower() == "bearer" else ""


class Gateway(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # BaseHTTPRequestHandler's own access log
        pass

    def do_GET(self):
        self.refuse_or_forward()

    def do_POST(self):
        self.refuse_or_forward()

    def do_PUT(self):
        self.refuse_or_forward()

    def do_DELETE(self):
        self.refuse_or_forward()

    def refuse_or_forward(self):
        if not (self.path == "/v1/messages" or self.path.startswith("/v1/messages/") or self.path.startswith("/v1/messages?")):
            self.drain()
            log(f"refuse method={self.command} path={self.path}")
            self.send_error(404, "not the Messages API")
            return
        self.forward()

    def drain(self):
        length = int(self.headers.get("Content-Length") or 0)
        while length > 0:
            chunk = self.rfile.read(min(length, 65536))
            if not chunk:
                break
            length -= len(chunk)

    def read_body(self):
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            return self.read_chunked()
        length = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(length) if length else b""

    def read_chunked(self):
        body = b""
        while True:
            size = int(self.rfile.readline().split(b";")[0].strip() or b"0", 16)
            if size == 0:
                while self.rfile.readline() not in (b"\r\n", b"\n", b""):
                    pass
                return body
            body += self.rfile.read(size)
            self.rfile.readline()

    def forward(self):
        inbound = bearer(self.headers.get("Authorization"))
        headers = {k: v for k, v in self.headers.items() if k.lower() not in HOP_BY_HOP}
        if SWAP:
            headers["Authorization"] = f"Bearer {SUBSCRIPTION_TOKEN}"
        headers["Host"] = UPSTREAM.netloc
        body = self.read_body()
        if body:
            headers["Content-Length"] = str(len(body))
        if DUMP and body:
            self.server.dumped = n = getattr(self.server, "dumped", 0) + 1
            with open(os.path.join(DUMP, f"{n}.json"), "wb") as f:
                f.write(body)
            with open(os.path.join(DUMP, f"{n}.headers"), "w") as f:
                f.writelines(f"{k}: {v}\n" for k, v in self.headers.items() if k.lower() != "authorization")
        names = ",".join(sorted(k.lower() for k in self.headers.keys()))
        log(f"request method={self.command} path={self.path} inbound-token={inbound or '-'} "
            f"outbound-host={UPSTREAM.hostname} swap={'yes' if SWAP else 'no'} headers={names}")

        conn_cls = http.client.HTTPSConnection if UPSTREAM.scheme == "https" else http.client.HTTPConnection
        kwargs = {"context": ssl.create_default_context()} if UPSTREAM.scheme == "https" else {}
        conn = conn_cls(UPSTREAM.hostname, UPSTREAM.port, timeout=600, **kwargs)
        try:
            conn.request(self.command, self.path, body=body, headers=headers)
            resp = conn.getresponse()
        except OSError as e:
            log(f"upstream-error inbound-token={inbound or '-'} outbound-host={UPSTREAM.hostname} error={e}")
            self.send_error(502, "upstream unreachable")
            return
        self.send_response(resp.status, resp.reason)
        for k, v in resp.getheaders():
            if k.lower() in HOP_BY_HOP or k.lower() == "content-length":
                continue
            self.send_header(k, v)
        self.send_header("Transfer-Encoding", "chunked")
        self.send_header("Connection", "close")
        self.end_headers()
        # Stream the body through as chunks, so SSE events reach the client as
        # they arrive rather than after the response completes.
        total = 0
        try:
            while True:
                chunk = resp.read1(65536) if hasattr(resp, "read1") else resp.read(65536)
                if not chunk:
                    break
                total += len(chunk)
                self.wfile.write(b"%x\r\n%s\r\n" % (len(chunk), chunk))
                self.wfile.flush()
            self.wfile.write(b"0\r\n\r\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            # The client abandoned the request mid-stream; the CLI does this
            # to the loser of its parallel requests. Not an error for us.
            log(f"client-closed inbound-token={inbound or '-'} outbound-host={UPSTREAM.hostname} "
                f"status={resp.status} bytes={total}")
        else:
            log(f"response inbound-token={inbound or '-'} outbound-host={UPSTREAM.hostname} status={resp.status} "
                f"bytes={total} content-type={resp.getheader('Content-Type', '-')}")
        finally:
            conn.close()
            self.close_connection = True


def main():
    if SWAP and not SUBSCRIPTION_TOKEN:
        sys.exit("CLAUDE_CODE_OAUTH_TOKEN is not set")
    host, _, port = os.environ.get("GATEWAY_BIND", "127.0.0.1:8080").rpartition(":")
    server = ThreadingHTTPServer((host, int(port)), Gateway)
    log(f"listening bind={host}:{port} upstream={UPSTREAM.scheme}://{UPSTREAM.netloc} swap={'yes' if SWAP else 'no'}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
