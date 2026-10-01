# Spec: Image generation through the gateway, and the MCP server that fronts it

**Status:** Draft
**Scope:** gateway `POST /v1/images/generations` + `/v1/images/edits`, frontend
routes, a `mflux` provider kind, and a thin MCP server (`deploy/dsh/mcp-imagegen/`)
that fronts the route for DSH.
**Depends on:** the existing mflux sidecar (`deploy/mflux-sidecar.py`), consumed
unchanged.
**Supersedes:** [lattice-image-generation.md](lattice-image-generation.md) §2 ("Why
not the gateway") and §8 ("Gateway/control-plane routing for images — explicitly
out"). Those sections are reversed by this spec.

---

## 1. Purpose

Image generation becomes a *routed Lattice modality*, not a sidecar-only bolt-on.
The gateway serves an **OpenAI Images API** surface (`POST /v1/images/generations`,
`POST /v1/images/edits`), reachable through the frontend and routed by control —
exactly as chat and embeddings are. A thin MCP server then fronts that route so DSH
(and any MCP client) can call it as model-callable tools. Lattice stays in control
of all local inference: the gateway's single slot and memory margin serialize image
generation against chat, and the control plane decides where it goes.

---

## 2. Why reverse the earlier decision

`lattice-image-generation.md` §2 rejected a gateway route on a *modality-boundary*
argument: the gateway's contract is "OpenAI chat + embeddings", and a diffusion
model has no `messages` / `tools` / `stream`. That argument cuts the other way once
the contract is named correctly. The **OpenAI Images API** (`/v1/images/generations`)
is not chat-shaped — but neither is `/v1/embeddings`, and the gateway already serves
that. The gateway's real contract is "OpenAI-shaped JSON over HTTP, one handler per
modality", and an image route is one more handler mirroring `handleEmbeddings`, plus
a provider that points at the already-running mflux sidecar.

The cost is a handler and a provider. The benefit is that image-gen becomes a
*routed* modality: one public entry point (frontend), one routing decision (control),
one local-inference arbiter (the gateway slot), one telemetry stream (the Bee log).
The sidecar-only path the old spec §4 chose never gave any of that — the sidecar is
reachable by anything on the tailnet, outside Lattice's memory/slot/telemetry
discipline. "Through the Lattice" now means through the gateway's router, which is
what the user asked for.

---

## 3. Architecture

```
DSH (Pi) ──mcp-client──► MCP server (Pi, stdio) ──POST /v1/images/generations──► frontend (:8080)
                                                                                 │  POST /route
                                                                                 ▼
                                                                         control (:8082) ──► mac-gateway (:8081)
                                                                                               │ acquireSlot + image memory margin
                                                                                               ▼
                                                                                         mflux sidecar (:8899) ──► FLUX.1-dev + LoRA
```

The MCP server is a *client of Lattice*, indistinguishable from any OpenAI Images
API caller. It sends no routing envelope; the image model's untagged name routes
local by the existing rule (§6).

---

## 4. The gateway route

Mirror `handleEmbeddings` exactly — it is the template for a new modality:

- `POST /v1/images/generations` — body is OpenAI Images (`model?`, `prompt`, `n?`,
  `size?`, `response_format?`). Translate to the sidecar's `/generate`; translate
  the sidecar's `{image: b64, seed, …}` back to `{created, data: [{b64_json}]}`.
- `POST /v1/images/edits` — `model?`, `image`, `prompt`, `mask?`, `n?`, `size?` →
  sidecar `/edit` (which adds `init_image`/`strength`).
- A new provider **kind `mflux`** in `providerConfigFile` (`providers.go`), with
  `endpoint` = the sidecar URL. `resolveModel` already routes by name; the image
  model is a `modelRoute` whose `provider` is the mflux entry. No change to the
  `Provider` interface's *chat* path — image gen is a sibling method/sibling
  provider, not a bend of `Execute`.
- **Same discipline as chat**, because this is local inference and the Mac is 16 GB:
  memory check → `acquireSlot` (the one local slot) → sidecar call → telemetry from a
  `defer` on every exit path. The sidecar has its own 409 single-flight lock, but the
  *gateway* slot is what serializes image-gen against chat; the sidecar lock is a
  second line of defense for direct sidecar consumers.
- **A dedicated image memory margin.** The chat margin (`marginMB` 1536 MiB) checks
  "room to load a 3–8 GB chat model"; the diffusion model is ~9 GB resident and needs
  a correspondingly larger threshold, or an image request will thrash swap. A distinct
  configurable margin, default ~10 GiB.
- **A dedicated timeout.** The gateway's `ollamaTimeout` (20 m) is wrong for images.
  The sidecar already uses `MFLUX_GEN_TIMEOUT` 3600 s; the gateway→sidecar client must
  match (≥ 3600 s), not inherit the chat timeout.
- **Announcement.** Add `image_generation` to `announcedCapabilities()` and the image
  model id (e.g. `flux-dev`, config-driven) to the announced Models list, so control's
  existing `selectGateway` finds it with no code change (§6).

---

## 5. The frontend route

`POST /v1/images/generations` + `/v1/images/edits` — a thin variant of
`proxyInference`. It parses the body enough to extract `model` (and any routing
envelope) for the control `/route` call, then proxies the *original* image body to
the gateway's image route (the chat body-rewriter `buildProxyBody` does not apply).
Same deferred telemetry; the image request joins the frontend→control→gateway stream
under one `request_id`.

---

## 6. Control: zero code change

This is the load-bearing finding of the investigation. Control's routing is already
data-driven for modality:

- `requiredCapability(privacy, latencyClass, model)` returns **`local`** for an
  untagged model name. A diffusion model's name (`flux-dev`) is untagged, so it is
  never cloud and never promoted to cloud — `LOCAL_ONLY` semantics hold unchanged.
- `selectGateway("local", "flux-dev")` filters on `hasCapability(gw.Capabilities,
  "local")` + `hostsModel(gw.Models, "flux-dev")` and matches `mac-gateway` once the
  gateway announces `flux-dev` in its Models (fact, from `/health`) and
  `image_generation` in its Capabilities. That is a *gateway config change*, adopted
  by `monitorHealth` — not a control-plane code change.

So the entire control plane is untouched. The routing decision for images is exactly
the same deterministic filter → score that chat uses, keyed on a model name.

---

## 7. The MCP server (primary subject)

A thin Python process using the official `mcp` SDK, run as a **stdio** child of
DSH's `mcp-client`. It exposes two tools:

- `generate_image(prompt, size?, n?)` → POSTs `/v1/images/generations`, returns the
  image as an MCP **image content block** (base64 PNG) — the "only durable rich-result
  bridge" DSH projects into native context.
- `generate_image_edit(image, prompt, size?)` → POSTs `/v1/images/edits`.

Placement, resolved in the decision log (§13): **Python stdio, on the Pi, fronting
the frontend** — not a gateway-native Go MCP endpoint. Three reasons:

1. **Protocol safety.** The official `mcp` SDK guarantees compatibility with the
   `@modelcontextprotocol/client` 2.x that DSH's `mcp-client` pins. Hand-rolling MCP
   in Go (~200 lines: `initialize`/`tools/list`/`tools/call`/`ping`, session-id
   headers, capability negotiation) risks subtle incompatibility with a living spec,
   and Lattice's posture is "fail loudly", not "probably interoperable".
2. **No listener.** stdio means DSH spawns the server on demand as a subprocess — no
   port, no supervision, no new long-running service. It is lighter than the mflux
   sidecar, which already pressed invariant 5 and was accepted.
3. **Full-path discipline.** Fronting the *frontend* (not the gateway, not the
   sidecar) keeps DSH's image traffic on the whole Lattice path — frontend → control →
   gateway — which is precisely "Lattice in control of all inference". Fronting the
   gateway directly would short-circuit control and drop the routing telemetry line.

The MCP server is *not* Lattice infrastructure and *not* subject to "Go stdlib only"
— that invariant scopes the Lattice binaries (gateway/control/frontend), and the MCP
server is a client adapter in the same category as the existing Python sidecars
(mflux, MLX) and the Hermes plugin. It is a committed artifact of the repo
(`deploy/dsh/mcp-imagegen/`), copied to the Pi by hand like the Hermes plugin.

---

## 8. DSH wiring

In `~/.dsh/profiles/*/cordis.patch.yml`, declare the `dsh-mcp-client` plugin with
`stdio` transport pointing at the MCP server entrypoint, and raise
`toolCallTimeoutMs` to ≥ 3,600,000 ms. This is mandatory: the default is 60,000 ms,
and a 512² image is ~10 minutes — every generation would be killed at 60 s.

---

## 9. Timeouts — the silent killer

Three hops need a ≥ 1-hour timeout, and none of the defaults provide it. A missed
one here is a *silent* failure (the generation runs, then the response is dropped and
the client times out).

| hop | default | needed |
|---|---|---|
| DSH mcp-client `toolCallTimeoutMs` | 60,000 ms | ≥ 3,600,000 ms |
| MCP server → frontend HTTP client | SDK default (seconds) | ≥ 3600 s |
| gateway → mflux sidecar | `ollamaTimeout` 20 m | dedicated, ≥ 3600 s |

The frontend→gateway hop is a `ReverseProxy` with no explicit timeout (fine), and the
frontend→control hop is ~5 s routing-only (fine).

---

## 10. Invariant check

| invariant | status |
|---|---|
| 1 — one public entry point | **held** — images enter via the frontend, like chat/embeddings |
| 2 — the Mac computes, the Pi routes | held — diffusion computes on the Mac; the Pi only routes and hosts the thin MCP adapter (no inference on the Pi) |
| 3 — `LOCAL_ONLY` never falls back | held — an untagged image model routes local, never promoted to cloud |
| 4 — the gateway translates, it does not decide | held — the gateway maps OpenAI Images ↔ sidecar and enforces slot/memory; control still decides the target |
| 5 — no new infrastructure | **pressed** — the MCP server is a new process, but stdio-spawned (no listener), and a client adapter, not Lattice infra. Same honest accounting as the mflux sidecar in the prior spec §9 |
| 6 — Bee is not modified | held |
| 7 — protect the Mac's SSD | **held** — the gateway slot serializes image vs chat (one resident model); the sidecar already runs `--low-ram --vae-tiling` |
| 8 — no usernames/hostnames/addresses | **held** — sidecar URL and MCP config are config-only, no committed defaults |
| 9 — telemetry shares one key | **held** — image requests ride the same `request_id` frontend→control→gateway stream |

---

## 11. Non-goals

- **Chat/embeddings/tool-calling via MCP** — out. This MCP server is image-only; the
  broader DSH chat surface is a separate, already-scoped problem.
- **Video generation** — out.
- **A gateway-native Go MCP endpoint** — recorded as a considered alternative, not
  chosen (§13). Easy to revisit: it is one more handler on the gateway.
- **Modifying the mflux sidecar** — it is consumed unchanged.
- **Image editing masks beyond what the sidecar supports** — the sidecar's `/edit`
  takes `init_image` + `strength`, not an OpenAI-style `mask`; the gateway maps what
  the sidecar can do and fails loudly on the rest.

---

## 12. Acceptance

1. Gateway `POST /v1/images/generations` returns `{data:[{b64_json}]}` for a prompt;
   a concurrent chat request queues behind the image request's slot (no concurrent
   resident models).
2. Frontend routes the request through control; control telemetry records
   `target: mac-gateway`, `model: flux-dev`, `capability: local`.
3. `LOCAL_ONLY` on an image request routes local (never cloud), and an irreconcilable
   request fails loudly by name.
4. The MCP server's `generate_image` returns an MCP image content block; DSH lists
   `mcp__<server>__generate_image` and a live DSH turn returns an image into native
   context without a 60 s timeout.

---

## 13. Decision log

- **2026-10-01 — reverse "not through the gateway".** Image generation becomes a
  routed modality via an OpenAI Images route, superseding `lattice-image-generation.md`
  §2/§8. Source: the user's direction ("fronts a future gateway /v1/images/generations
  route… the lattice to remain in control of all inference").
- **2026-10-01 — control plane needs no code change.** Routing is data-driven on the
  model tag; an untagged diffusion model routes local through the existing
  `requiredCapability`/`selectGateway`. The only wiring is the gateway announcing the
  image model + `image_generation` capability in `/health`.
- **2026-10-01 — MCP placement: Python stdio, fronting the frontend.** Chosen over a
  gateway-native Go MCP endpoint for protocol safety (official SDK vs hand-rolled
  JSON-RPC against a pinned client), no-listener operation (stdio spawn), and
  full-path discipline (frontend→control→gateway). Reversible: a gateway-native MCP
  handler is a bounded alternative if "Go stdlib only, one process" is preferred over
  the SDK's compatibility guarantee.
