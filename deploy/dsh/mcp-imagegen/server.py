#!/usr/bin/env python3
"""Lattice image-generation MCP server — fronts Lattice's /v1/images routes.

Run as a stdio child of DSH's dsh-mcp-client (transport: stdio). It exposes two
model-callable tools, generate_image and generate_image_edit, each of which POSTs
to the Lattice frontend's OpenAI Images route and returns the image as an MCP
image content block. It computes nothing itself: Lattice's gateway owns the
single slot, the memory margin and the routing; this process is only the
protocol adapter that turns an MCP tool call into an HTTP POST.

Requires the `mcp` package:  pip install mcp
Run:                          python server.py   (stdio)
"""

from __future__ import annotations

import json
import os
import urllib.request

from mcp.server.mcpserver import MCPServer
from mcp.types import ImageContent

FRONTEND = os.environ.get("LATTICE_FRONTEND_URL", "http://127.0.0.1:8080")
# Images are named in the gateway registry (lattice model names, not OpenAI's);
# the launch env picks the host's default, the code only carries a fallback.
DEFAULT_MODEL = os.environ.get("LATTICE_IMAGEGEN_MODEL", "flux-dev")
TIMEOUT = int(os.environ.get("LATTICE_IMAGE_TIMEOUT_S", "7200"))

mcp = MCPServer("lattice-imagegen")


def _image(b64: str) -> ImageContent:
    return ImageContent(type="image", data=b64, mime_type="image/png")


def _model(model: str) -> str:
    return model.strip() if model and model.strip() else DEFAULT_MODEL


def _loras(loras: str) -> list[dict]:
    """Comma-separated "name:scale" specs ("lustly:0.8, other:1.0") to the
    gateway's [{name, scale}] array. A spec without a scale is 1.0. Empty input
    is an empty list — absent, the gateway forwards nothing."""
    out = []
    for spec in (loras or "").split(","):
        spec = spec.strip()
        if not spec:
            continue
        name, _, scale = spec.partition(":")
        try:
            out.append({"name": name.strip(), "scale": float(scale) if scale.strip() else 1.0})
        except ValueError:
            continue
    return out


def _generate(prompt: str, size: str | None, n: int, model: str, loras: list[dict]) -> list[ImageContent]:
    body = {"model": _model(model), "prompt": prompt, "n": n, "response_format": "b64_json"}
    if size:
        body["size"] = size
    if loras:
        body["loras"] = loras
    req = urllib.request.Request(
        f"{FRONTEND}/v1/images/generations",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
        payload = json.load(resp)
    return [_image(item["b64_json"]) for item in payload["data"]]


def _edit(image_b64: str, prompt: str, size: str | None, n: int, model: str, loras: list[dict]) -> list[ImageContent]:
    body = {"model": _model(model), "image": image_b64, "prompt": prompt, "n": n, "response_format": "b64_json"}
    if size:
        body["size"] = size
    if loras:
        body["loras"] = loras
    req = urllib.request.Request(
        f"{FRONTEND}/v1/images/edits",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
        payload = json.load(resp)
    return [_image(item["b64_json"]) for item in payload["data"]]


@mcp.tool()
def generate_image(prompt: str, size: str = "512x512", n: int = 1, model: str = "", loras: str = "") -> list[ImageContent]:
    """Generate an image from a text prompt via Lattice's image route.

    size is WxH in pixels ("256x256", "512x512", "1024x1024"); smaller is much
    faster. n is the number of images. model is a registry name (e.g.
    "flux-dev", "z-image-turbo", "qwen-image-2.1", "flux-uncensored"); empty
    uses the gateway's configured default. loras is optional, comma-separated
    "name:scale" pairs ("lustly:0.8") naming LoRA files the target sidecar
    knows; empty means none. Returns the image(s) as PNG content.
    """
    return _generate(prompt, size, n, model, _loras(loras))


@mcp.tool()
def generate_image_edit(image_b64: str, prompt: str, size: str = "512x512", n: int = 1, model: str = "", loras: str = "") -> list[ImageContent]:
    """Edit an existing image (base64) guided by a text prompt, via Lattice's image route.

    model is an optional registry name; empty uses the gateway's default.
    loras is optional, comma-separated "name:scale" pairs; empty means none.
    """
    return _edit(image_b64, prompt, size, n, model, _loras(loras))


if __name__ == "__main__":
    mcp.run()
