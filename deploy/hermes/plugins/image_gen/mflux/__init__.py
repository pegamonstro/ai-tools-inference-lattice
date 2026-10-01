"""MFLUX (local) image generation backend — FLUX.1-dev + Lustly.ai uncensored LoRA via an MLX sidecar.

The sidecar runs on the Mac (the only MLX + RAM host) and is driven over the tailnet.
Unlike the cloud providers there is no API key: availability is the presence of a
configured sidecar URL, and generation happens on local hardware. Text-to-image and
image-to-image (edit) both route through the sidecar.

Configuration (Hermes ``config.yaml``):

    image_gen:
      provider: mflux
      mflux:
        url: http://<mac-tailnet>:8899   # required; no default, per the no-address rule
        token: ""                         # optional; must match the sidecar's MFLUX_TOKEN

The plugin's ``is_available()`` deliberately does NOT probe the sidecar — the picker
calls it on every paint and must not block on the network; a down sidecar surfaces as
a connection error at generation time instead.
"""

from __future__ import annotations

import base64
import os
from typing import Any, Dict, List, Optional

import requests

from agent.image_gen_provider import (
    DEFAULT_ASPECT_RATIO, ImageGenProvider, error_response, resolve_aspect_ratio,
    save_b64_image, success_response,
)
from plugins.image_gen._common import load_image_gen_config

logger = __import__("logging").getLogger(__name__)

_MODEL_ID = "flux-uncensored"

# FLUX.1-dev aspect ratios (multiples of 64, ~1MP) for the three Hermes aspects.
_ASPECT_SIZES = {
    "landscape": (1344, 768),
    "square": (1024, 1024),
    "portrait": (768, 1344),
}

# Must outlast the sidecar's own GEN_TIMEOUT (3600) — the plugin waits on the backend
# it drives, and a 4-step image can exceed 30 min when the M1 is under load.
_GEN_TIMEOUT = 3600


def _sidecar_url() -> Optional[str]:
    cfg = load_image_gen_config("mflux")
    raw = os.environ.get("MFLUX_SIDECAR_URL") or cfg.get("url") or cfg.get("endpoint")
    return raw.strip().rstrip("/") if isinstance(raw, str) and raw.strip() else None


def _sidecar_token() -> str:
    cfg = load_image_gen_config("mflux")
    raw = os.environ.get("MFLUX_SIDECAR_TOKEN") or cfg.get("token")
    return raw.strip() if isinstance(raw, str) and raw.strip() else ""


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
    """Local uncensored diffusion backend delegating to the Mac-side mflux sidecar."""

    @property
    def name(self) -> str:
        return "mflux"

    @property
    def display_name(self) -> str:
        return "MFLUX (local)"

    def is_available(self) -> bool:
        # No network call (the picker calls this on every paint): available iff the
        # sidecar URL is configured. A down sidecar is a runtime error, not absence.
        return _sidecar_url() is not None

    def list_models(self) -> List[Dict[str, Any]]:
        return [{
            "id": _MODEL_ID,
            "display": "Flux Uncensored (Lustly.ai v1, local MLX)",
            "speed": "~2–5 min on M1 16 GB",
            "strengths": "Uncensored text-to-image & editing, fully local",
            "price": "local",
        }]

    def default_model(self) -> Optional[str]:
        return _MODEL_ID

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
        url = _sidecar_url()
        if url is None:
            return error_response(
                error="mflux sidecar is not configured (set image_gen.mflux.url)",
                error_type="configuration", provider="mflux", prompt=prompt,
                aspect_ratio=aspect_ratio,
            )

        aspect = resolve_aspect_ratio(aspect_ratio)
        width, height = _ASPECT_SIZES[aspect]

        payload: Dict[str, Any] = {"prompt": prompt, "width": width, "height": height}
        for key in ("negative_prompt", "steps", "guidance", "seed"):
            if kwargs.get(key) is not None:
                payload[key] = kwargs[key]

        is_edit = bool(image_url)
        if is_edit:
            try:
                payload["init_image"] = base64.b64encode(_image_bytes(image_url)).decode("ascii")
            except Exception as exc:  # noqa: BLE001
                return error_response(
                    error=f"could not read source image: {exc}", error_type="invalid_argument",
                    provider="mflux", model=_MODEL_ID, prompt=prompt, aspect_ratio=aspect,
                )
            if kwargs.get("strength") is not None:
                payload["strength"] = kwargs["strength"]

        headers = {}
        token = _sidecar_token()
        if token:
            headers["X-Mflux-Token"] = token

        try:
            resp = requests.post(f"{url}/{'edit' if is_edit else 'generate'}", json=payload, headers=headers, timeout=_GEN_TIMEOUT)
            resp.raise_for_status()
            data = resp.json()
        except requests.Timeout:
            return error_response(
                error="mflux generation timed out", error_type="timeout",
                provider="mflux", model=_MODEL_ID, prompt=prompt, aspect_ratio=aspect,
            )
        except requests.RequestException as exc:
            return error_response(
                error=f"mflux sidecar unreachable: {exc}", error_type="connection_error",
                provider="mflux", model=_MODEL_ID, prompt=prompt, aspect_ratio=aspect,
            )
        except ValueError as exc:
            return error_response(
                error=f"mflux sidecar returned invalid JSON: {exc}", error_type="invalid_response",
                provider="mflux", model=_MODEL_ID, prompt=prompt, aspect_ratio=aspect,
            )

        if not data.get("image"):
            return error_response(
                error=data.get("error") or "mflux returned no image",
                error_type=data.get("error_type") or "provider_error",
                provider="mflux", model=_MODEL_ID, prompt=prompt, aspect_ratio=aspect,
            )

        try:
            path = str(save_b64_image(data["image"], prefix="mflux"))
        except Exception as exc:  # noqa: BLE001
            return error_response(
                error=f"could not save image to cache: {exc}", error_type="io_error",
                provider="mflux", model=_MODEL_ID, prompt=prompt, aspect_ratio=aspect,
            )

        extra = {"seed": data["seed"]} if data.get("seed") is not None else None
        return success_response(
            image=path, model=_MODEL_ID, prompt=prompt, aspect_ratio=aspect,
            provider="mflux", extra=extra,
        )


def register(ctx) -> None:
    """Plugin entry point — wire MfluxImageGenProvider into the registry."""
    ctx.register_image_gen_provider(MfluxImageGenProvider())
