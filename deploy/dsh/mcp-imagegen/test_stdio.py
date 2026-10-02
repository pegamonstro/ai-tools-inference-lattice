#!/usr/bin/env python3
"""Stdio MCP handshake smoke test — exercises the real protocol layer.

Complements test_server.py, which bypasses MCP and calls server._generate
directly. This harness spawns server.py as a stdio child and drives the full
JSON-RPC sequence — initialize -> notifications/initialized -> tools/list ->
tools/call — over newline-delimited JSON, asserting the tool metadata and the
image content block. It fronts a stdlib mock of the Lattice frontend so no real
Lattice or FLUX model is needed.

Run:  .venv/bin/python test_stdio.py   (prints "ok")
"""

import http.server
import json
import os
import subprocess
import sys
import threading

HERE = os.path.dirname(os.path.abspath(__file__))
PROTOCOL_VERSION = "2026-07-28"  # mcp SDK's LATEST_PROTOCOL_VERSION

_mock = {}


class _Frontend(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        _mock["path"] = self.path
        _mock["body"] = json.loads(self.rfile.read(length) or b"{}")
        body = json.dumps({"created": 1, "data": [{"b64_json": "aGVsbG8="}]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


def _send(proc, msg):
    proc.stdin.write(json.dumps(msg) + "\n")
    proc.stdin.flush()


def _recv(proc):
    line = proc.stdout.readline()
    if not line:
        raise RuntimeError("server closed stdout before responding")
    return json.loads(line)


def _request(proc, request_id, method, params=None):
    """Send a request and return the response with the matching id, skipping
    server-initiated notifications (e.g. progress/log) that may interleave."""
    msg = {"jsonrpc": "2.0", "id": request_id, "method": method}
    if params is not None:
        msg["params"] = params
    _send(proc, msg)
    while True:
        resp = _recv(proc)
        if resp.get("id") == request_id:
            return resp


def main():
    # 1. Mock frontend on a random port.
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _Frontend)
    threading.Thread(target=srv.serve_forever, daemon=True).start()

    env = dict(os.environ)
    env["LATTICE_FRONTEND_URL"] = f"http://127.0.0.1:{srv.server_port}"
    env["LATTICE_IMAGE_TIMEOUT_S"] = "5"

    # 2. Spawn the server as a stdio child (venv python has `mcp` importable).
    proc = subprocess.Popen(
        [sys.executable, os.path.join(HERE, "server.py")],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        env=env, text=True,
    )

    try:
        # 3. initialize
        init = _request(proc, 1, "initialize", {
            "protocolVersion": PROTOCOL_VERSION,
            "capabilities": {},
            "clientInfo": {"name": "test_stdio", "version": "0.0.0"},
        })
        assert "result" in init, init
        result = init["result"]
        # The server negotiates the version (returns its own supported version,
        # not an echo of ours); only assert a successful, non-empty negotiation.
        assert result.get("protocolVersion"), result
        assert result.get("serverInfo", {}).get("name") == "lattice-imagegen", result

        # 4. initialized notification
        _send(proc, {"jsonrpc": "2.0", "method": "notifications/initialized"})

        # 5. tools/list — both tools advertised, not disabled.
        listed = _request(proc, 2, "tools/list")
        names = {t["name"] for t in listed["result"]["tools"]}
        assert {"generate_image", "generate_image_edit"} <= names, names

        # 6. tools/call generate_image — returns one image content block.
        called = _request(proc, 3, "tools/call", {
            "name": "generate_image",
            "arguments": {"prompt": "a cat", "size": "512x512", "n": 1},
        })
        assert not called["result"].get("isError"), called
        content = called["result"]["content"]
        assert len(content) == 1, content
        block = content[0]
        assert block["type"] == "image", block
        assert block["data"] == "aGVsbG8=", block

        # 7. The mock frontend received the correct OpenAI Images POST.
        assert _mock["path"] == "/v1/images/generations", _mock
        assert _mock["body"] == {
            "model": "flux-dev", "prompt": "a cat", "n": 1,
            "response_format": "b64_json", "size": "512x512",
        }, _mock["body"]

        print("ok")
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()


if __name__ == "__main__":
    main()
