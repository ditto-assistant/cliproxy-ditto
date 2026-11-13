"""Fake upstreams for the end-to-end test.

One process serves two ports: a "subscription" that speaks Anthropic Messages
(exhausted or healthy, per SUB_MODE) and a fake Ditto Router. Every request is
appended to a JSONL log so the test asserts on what actually arrived.
"""

import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG = sys.argv[3]
lock = threading.Lock()


def record(who, handler, body):
    entry = {
        "who": who,
        "path": handler.path,
        "authorization": handler.headers.get("Authorization", ""),
        "x_api_key": handler.headers.get("x-api-key", ""),
        "via": handler.headers.get("X-Ditto-Via", ""),
        "anthropic_version": handler.headers.get("anthropic-version", ""),
        "request_class": handler.headers.get("x-claude-code-request-class", ""),
        "body": body.decode("utf-8", "replace"),
    }
    with lock, open(LOG, "a") as f:
        f.write(json.dumps(entry) + "\n")


def message(text):
    return {
        "id": "msg_e2e",
        "type": "message",
        "role": "assistant",
        "model": "claude-opus-5-5",
        "content": [{"type": "text", "text": text}],
        "stop_reason": "end_turn",
        "stop_sequence": None,
        "usage": {"input_tokens": 3, "output_tokens": 2},
    }


def sse(text):
    events = [
        ("message_start", {"type": "message_start", "message": {**message(""), "content": [], "stop_reason": None}}),
        ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
        ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": text}}),
        ("content_block_stop", {"type": "content_block_stop", "index": 0}),
        ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 2}}),
        ("message_stop", {"type": "message_stop"}),
    ]
    return "".join(f"event: {name}\ndata: {json.dumps(data)}\n\n" for name, data in events).encode()


class Base(BaseHTTPRequestHandler):
    who = ""

    def log_message(self, *args):
        pass

    def body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

    def send(self, status, payload, ctype="application/json"):
        raw = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


class Subscription(Base):
    def do_POST(self):
        body = self.body()
        record("subscription", self, body)
        if os.environ.get("SUB_MODE") == "exhausted":
            self.send(429, {"type": "error", "error": {"type": "rate_limit_error", "message": "usage limit reached"}})
            return
        req = json.loads(body or b"{}")
        if req.get("stream"):
            self.send(200, sse("from-subscription"), "text/event-stream")
        else:
            self.send(200, message("from-subscription"))

    do_GET = do_POST


class Ditto(Base):
    def do_GET(self):
        record("ditto", self, b"")
        if self.headers.get("Authorization") != "Bearer ditto_inf_e2e":
            self.send(401, {"error": {"message": "bad key"}})
        elif self.path == "/v1/models":
            self.send(200, {"object": "list", "data": [
                {"id": "ditto-dev", "object": "model", "owned_by": "ditto"},
                {"id": "anthropic/claude-opus-4.5", "object": "model", "owned_by": "anthropic"},
            ]})
        elif self.path == "/v1/credits":
            self.send(200, {"data": {"currency": "usd", "billing_mode": "ditto", "balance": 42.0,
                            "endpoint": {"limit": 10, "limit_remaining": 4, "limit_reset": "monthly", "usage": 6}}})
        else:
            self.send(404, {"error": {"message": "no route"}})

    def do_POST(self):
        body = self.body()
        record("ditto", self, body)
        if self.headers.get("Authorization") != "Bearer ditto_inf_e2e":
            self.send(401, {"error": {"message": "bad key"}})
            return
        mode = os.environ.get("DITTO_MODE", "ok")
        if mode == "400":
            self.send(400, {"type": "error", "error": {"type": "invalid_request_error", "message": "provider returned HTTP 400: prompt is too long"}})
            return
        if mode == "503":
            self.send(503, {"type": "error", "error": {"type": "overloaded_error", "message": "provider overloaded"}})
            return
        req = json.loads(body or b"{}")
        if mode == "empty":
            self.send(200, {**message(""), "stop_reason": "max_tokens"})
        elif req.get("stream"):
            self.send(200, sse("from-ditto"), "text/event-stream")
        else:
            self.send(200, message("from-ditto"))


def serve(port, handler):
    ThreadingHTTPServer(("127.0.0.1", port), handler).serve_forever()


threading.Thread(target=serve, args=(int(sys.argv[1]), Subscription), daemon=True).start()
serve(int(sys.argv[2]), Ditto)
