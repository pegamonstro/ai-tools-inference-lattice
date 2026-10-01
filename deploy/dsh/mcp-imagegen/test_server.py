#!/usr/bin/env python3
"""Smoke test for the MCP server's HTTP adapter — no MCP client needed.

Spins up a stdlib mock of the Lattice frontend and asserts that generate_image's
helper POSTs the OpenAI Images body and turns the response into image content
blocks. Run:  python3 test_server.py
"""

import http.server
import json
import os
import threading

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


def main():
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _Frontend)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    os.environ["LATTICE_FRONTEND_URL"] = f"http://127.0.0.1:{srv.server_port}"
    os.environ["LATTICE_IMAGE_TIMEOUT_S"] = "5"

    import server  # reads the env above at import time

    blocks = server._generate("a cat", "512x512", 1)
    assert _mock["path"] == "/v1/images/generations", _mock["path"]
    assert _mock["body"] == {"model": "flux-dev", "prompt": "a cat", "n": 1,
                             "response_format": "b64_json", "size": "512x512"}, _mock["body"]
    assert len(blocks) == 1
    assert blocks[0].type == "image"
    assert blocks[0].data == "aGVsbG8="
    print("ok")


if __name__ == "__main__":
    main()
