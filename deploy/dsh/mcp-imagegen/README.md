# lattice-imagegen — MCP server for Lattice image generation

A stdio [MCP](https://modelcontextprotocol.io) server that fronts Lattice's
OpenAI Images route so DSH (or any MCP client) can generate and edit images as
model-callable tools. It computes nothing: the Lattice gateway owns the slot,
the memory margin and the routing.

## Install

```bash
python3 -m venv .venv
.venv/bin/pip install mcp
```

## Run (stdio)

```bash
LATTICE_FRONTEND_URL=http://127.0.0.1:8080 .venv/bin/python server.py
```

The server reads stdin/stdout; DSH spawns it as a subprocess.

## Tools

- `generate_image(prompt, size="512x512", n=1)` → PNG image content.
- `generate_image_edit(image_b64, prompt, size="512x512", n=1)` → PNG image content.

## Configuration

- `LATTICE_FRONTEND_URL` — the Lattice frontend (default `http://127.0.0.1:8080`).
- `LATTICE_IMAGE_TIMEOUT_S` — HTTP client timeout (default `7200`); must exceed a
  1024² generation (~1 h).

## Test

```bash
python3 test_server.py   # prints "ok"
```
