"""Lattice image generation backend — every registry model, through the lattice frontend.

Unlike the cloud providers there is no API key: availability is the presence of a
frontend URL, and generation happens on local hardware. The plugin is a thin OpenAI
Images client: it POSTs to the frontend's /v1/images routes with a registry model
name, and the lattice owns the rest — control picks the gateway, the gateway picks
the sidecar instance (via the model's upstream mapping), the slot and memory margin
apply, and one telemetry line per request flows. Text-to-image and image-to-image
(edit) both route through the frontend; the edit's base64 rides the request.

Configuration (Hermes ``config.yaml``):

    image_gen:
      provider: mflux
      mflux:
        url: http://127.0.0.1:8080   # required; the lattice frontend, per the no-address rule
        model: flux-uncensored         # optional; a registry model name for the default
        size: small                    # optional; "small" (⅓–¼ px, default) or "medium" (~1MP)

Model ids are registry names (flux-uncensored, z-image-turbo, qwen-image-2.1, …),
the same names the gateway registry — host runtime ``gateway-providers.json`` —
carries. An unset model omits the field and the gateway's default applies.

The plugin's ``is_available()`` deliberately does NOT probe the frontend — the picker
calls it on every paint and must not block on the network; a down lattice surfaces as
a connection error at generation time instead.
"""

from __future__ import annotations

import base64
import os
import time
from typing import Any, Dict, List, Optional

import requests

from agent.image_gen_provider import (
    DEFAULT_ASPECT_RATIO, ImageGenProvider, error_response, resolve_aspect_ratio,
    save_b64_image, success_response,
)
from plugins.image_gen._common import load_image_gen_config

logger = __import__("logging").getLogger(__name__)

# The gateway's own default when a request omits the model entirely.
_GATEWAY_DEFAULT_MODEL = "flux-dev"

# Sizes (multiples of 64) for the three Hermes aspects. Pixel count is the
# load lever: the diffusion transformer denoises the whole canvas regardless of
# prompt, so "small" (⅓–¼ the pixels) is what cuts compute and memory on the
# 16 GB host.
_SIZES = {
    "small":  {"landscape": (768, 448), "square": (512, 512), "portrait": (448, 768)},
    "medium": {"landscape": (1344, 768), "square": (1024, 1024), "portrait": (768, 1344)},
}
_DEFAULT_SIZE = "small"

# Must outlast the sidecar's own GEN_TIMEOUT (3600) — the plugin waits on the
# backend it drives, and a 4-step image can exceed 30 min when a host is under load.
_GEN_TIMEOUT = 3600

# Lattice extensions the gateway forwards to the sidecar (not OpenAI fields).
_TUNABLES = ("negative_prompt", "steps", "guidance", "seed")

# The picker re-renders on every paint, so the live zoo read is cached this
# long: fresh enough for a registry edited minutes ago at setup time, cheap
# enough not to open a request per keystroke.
_ZOO_TTL_S = 30
_zoo_cache: dict = {}


def _frontend_url() -> Optional[str]:
    cfg = load_image_gen_config("mflux")
    raw = os.environ.get("LATTICE_FRONTEND_URL") or cfg.get("url") or cfg.get("endpoint")
    return raw.strip().rstrip("/") if isinstance(raw, str) and raw.strip() else None


def _frontend_loras() -> Optional[List[Dict[str, Any]]]:
    """LoRAs configured for this backend (`image_gen.mflux.loras:`), in the
    gateway's `[{name, scale}]` shape. Config-level, not per-request: the core
    image_generate schema advertises no loras arg, so the knob is a setup-time
    decision like the model default. Absent stays absent — a strict no-op."""
    cfg = load_image_gen_config("mflux")
    loras = cfg.get("loras")
    return loras if isinstance(loras, list) and loras else None


def _frontend_url() -> Optional[str]:
    cfg = load_image_gen_config("mflux")
    raw = os.environ.get("LATTICE_FRONTEND_URL") or cfg.get("url") or cfg.get("endpoint")
    return raw.strip().rstrip("/") if isinstance(raw, str) and raw.strip() else None


def _zoo() -> List[str]:
    """Registry names live from the frontend's /v1/images/models. Read failures
    are cached as empty for the TTL like successes: the picker falls back to
    the configured default and the next read past the TTL retries."""
    now = time.monotonic()
    if _zoo_cache.get("at") is not None and now - _zoo_cache["at"] < _ZOO_TTL_S:
        return _zoo_cache.get("ids") or []
    ids: List[str] = []
    url = _frontend_url()
    if url:
        try:
            resp = requests.get(f"{url}/v1/images/models", timeout=5)
            resp.raise_for_status()
            ids = [m["id"] for m in (resp.json().get("data") or []) if isinstance(m, dict) and m.get("id")]
        except Exception:  # noqa: BLE001 — any failure degrades to the fallback entry
            ids = []
    _zoo_cache.update(at=now, ids=ids)
    return ids


def _configured_model() -> Optional[str]:
    cfg = load_image_gen_config("mflux")
    raw = os.environ.get("LATTICE_IMAGEGEN_MODEL") or cfg.get("model")
    return raw.strip() if isinstance(raw, str) and raw.strip() else None


def _configured_size() -> Optional[str]:
    cfg = load_image_gen_config("mflux")
    raw = cfg.get("size")
    return raw.strip() if isinstance(raw, str) and raw.strip() else None


def _resolve_size(value: Optional[str]) -> str:
    """Clamp a size request to ``_SIZES``; unknown values coerce to the default."""
    v = value.strip().lower() if isinstance(value, str) else ""
    return v if v in _SIZES else _DEFAULT_SIZE


def _image_bytes(source: str) -> bytes:
    """Resolve an ``image_url`` (data URL, http(s) URL, or local path) to bytes."""
    s = source.strip()
    if s.startswith("data:"):
        head, _, data = s.partition(",")
        return base64.b64decode(data) if ";base64" in head else data.encode()
    if s.startswith(("http://", "https://")):
        resp = requests.get(s, timeout=60)
        resp.raise_for_status()
        return resp.content
    with open(s, "rb") as fh:
        return fh.read()


class MfluxImageGenProvider(ImageGenProvider):
    """Local diffusion backend delegating every model to the lattice image route."""

    @property
    def name(self) -> str:
        return "mflux"

    @property
    def display_name(self) -> str:
        return "Lattice (local image)"

    def is_available(self) -> bool:
        # No network call (the picker calls this on every paint): available iff the
        # frontend URL is configured. A down lattice is a runtime error, not absence.
        return _frontend_url() is not None

    def list_models(self) -> List[Dict[str, Any]]:
        # The live zoo, read from the frontend's image registry route. Falls
        # back to the configured default when the read fails, so a picker never
        # goes empty from a transient control-plane hiccup — one entry beats a
        # blank menu, and the next successful read refreshes the zoo.
        zoo = _zoo()
        if zoo:
            return [{
                "id": mid,
                "display": f"{mid} (registry name, via lattice)",
                "speed": "~10 s on M6 (32 GB), ~2 min on M1 (16 GB)",
                "strengths": "Text-to-image & editing, fully local, multi-model",
                "price": "local",
            } for mid in zoo]
        model = _configured_model() or _GATEWAY_DEFAULT_MODEL
        return [{
            "id": model,
            "display": f"{model} (registry name, via lattice)",
            "speed": "~10 s on M6 (32 GB), ~2 min on M1 (16 GB)",
            "strengths": "Text-to-image & editing, fully local, multi-model",
            "price": "local",
        }]

    def default_model(self) -> Optional[str]:
        return _configured_model() or _GATEWAY_DEFAULT_MODEL

    def capabilities(self) -> Dict[str, Any]:
        return {"modalities": ["text", "image"], "max_reference_images": 1}

    def generate(
        self,
        prompt: str,
        aspect_ratio: str = DEFAULT_ASPECT_RATIO,
        *,
        image_url: Optional[str] = None,
        reference_image_urls: Optional[List[str]] = None,
        **kwargs: Any,
    ) -> Dict[str, Any]:
        url = _frontend_url()
        if url is None:
            return error_response(
                error="lattice frontend is not configured (set image_gen.mflux.url)",
                error_type="configuration", provider="mflux", prompt=prompt,
                aspect_ratio=aspect_ratio,
            )

        aspect = resolve_aspect_ratio(aspect_ratio)
        size = _resolve_size(kwargs.get("size") or _configured_size())
        width, height = _SIZES[size][aspect]

        # OpenAI Images shape, route names a registry model; the tunables ride
        # the same body as Lattice extensions the gateway forwards downstream.
        payload: Dict[str, Any] = {"prompt": prompt, "size": f"{width}x{height}", "n": 1}
        # The picker's choice reaches the provider as `model` (the core
        # dispatcher reads image_gen.model): it wins over this backend's own
        # default, or the pick the user made would be a silent no-op.
        model = kwargs.get("model") or _configured_model()
        if model:
            payload["model"] = model
        for key in _TUNABLES:
            if kwargs.get(key) is not None:
                payload[key] = kwargs[key]
        loras = _frontend_loras()
        if loras:
            payload["loras"] = loras

        is_edit = bool(image_url)
        if is_edit:
            try:
                payload["image"] = base64.b64encode(_image_bytes(image_url)).decode("ascii")
            except Exception as exc:  # noqa: BLE001
                return error_response(
                    error=f"could not read source image: {exc}", error_type="invalid_argument",
                    provider="mflux", model=model or _GATEWAY_DEFAULT_MODEL, prompt=prompt, aspect_ratio=aspect,
                )
            if kwargs.get("strength") is not None:
                payload["strength"] = kwargs["strength"]

        try:
            resp = requests.post(
                f"{url}/v1/images/{'edits' if is_edit else 'generations'}",
                json=payload, timeout=_GEN_TIMEOUT,
            )
            resp.raise_for_status()
            data = resp.json()
        except requests.Timeout:
            return error_response(
                error="lattice image generation timed out", error_type="timeout",
                provider="mflux", model=model or _GATEWAY_DEFAULT_MODEL, prompt=prompt, aspect_ratio=aspect,
            )
        except requests.RequestException as exc:
            return error_response(
                error=f"lattice unreachable: {exc}", error_type="connection_error",
                provider="mflux", model=model or _GATEWAY_DEFAULT_MODEL, prompt=prompt, aspect_ratio=aspect,
            )
        except ValueError as exc:
            return error_response(
                error=f"lattice returned invalid JSON: {exc}", error_type="invalid_response",
                provider="mflux", model=model or _GATEWAY_DEFAULT_MODEL, prompt=prompt, aspect_ratio=aspect,
            )

        items = (data or {}).get("data") or []
        if not items or not items[0].get("b64_json"):
            return error_response(
                error=(data or {}).get("error") or "lattice returned no image",
                error_type=(data or {}).get("error_type") or "provider_error",
                provider="mflux", model=model or _GATEWAY_DEFAULT_MODEL, prompt=prompt, aspect_ratio=aspect,
            )

        try:
            path = str(save_b64_image(items[0]["b64_json"], prefix="mflux"))
        except Exception as exc:  # noqa: BLE001
            return error_response(
                error=f"could not save image to cache: {exc}", error_type="io_error",
                provider="mflux", model=model or _GATEWAY_DEFAULT_MODEL, prompt=prompt, aspect_ratio=aspect,
            )

        return success_response(
            image=path, model=model or _GATEWAY_DEFAULT_MODEL, prompt=prompt, aspect_ratio=aspect,
            provider="mflux", extra={"size": size},
        )


def register(ctx) -> None:
    """Plugin entry point — wire MfluxImageGenProvider into the registry."""
    ctx.register_image_gen_provider(MfluxImageGenProvider())