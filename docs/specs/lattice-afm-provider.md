# Spec: Apple Foundation Models (AFM) Provider — future lattice kind `"afm"`

**Status:** Spec only (LAT-AFM-0) — docs; no code, no config, no deploy, no restart
**Host:** Mac M6 (on-device AFM) behind Lattice Gateway; clients reach Lattice only via the RPi4 frontend
**Gate:** three-layer kill switch **default OFF** (§4); acceptance-before-enable (§7) must pass before any enable flip
**Provenance:** CoS receipt `DECISIONS/CoS-20261120T062843Z-t1006s4109-lat-url-docs-afm-0.md`, sourced from `~/work/seed-fm-research-20261119/AFM-LEGAL-AND-LATTICE.md` Part C (sha256 prefix `077b714f`). This file is the lattice-repo landing of that brief.

This document scopes a **future** Gateway provider kind `"afm"` for Apple Foundation Models running on-device on the M6. It does **not** authorise enabling AFM, standing up a helper, editing `LATTICE_GATEWAY_PROVIDERS`, or changing S.E.E.D. routing. Plan/spec only.

Related existing specs:

- [`lattice-mlx-provider.md`](lattice-mlx-provider.md) — the live multi-provider registry pattern AFM must extend
- [`lattice-apple-acceleration.md`](lattice-apple-acceleration.md) — M6 / MLX / Neural Accelerator context
- [`lattice-control.md`](lattice-control.md) — control routing and the no-cloud-fallback sovereignty rule
- [`lattice-failure.md`](lattice-failure.md) — fail-closed `LOCAL_ONLY` semantics

---

## Licence conditions (verbatim; normative)

The following conditions are **verbatim** from the CoS brief (LAT-AFM-0 / Part C) and bind every future enablement of AFM through Lattice. They are not paraphrased:

1. **personal non-commercial use only**
2. **AFM listener local-only (Unix socket 0600 or 127.0.0.1), never `fm serve` or the helper on a network port**
3. **concurrency 1**
4. **no AFM outputs used to train, fine-tune, distil or embed any model**
5. **no portal or X-Wars use**
6. **human review before CANONICAL**
7. **provenance tag**
8. **S.E.E.D. reaches it only via `http://100.80.26.89:8080/v1`**

Apple’s public Acceptable Use Requirements for the Foundation Models framework remain in force in addition to the above:
<https://developer.apple.com/support/terms/acceptable-use-requirements-for-the-foundation-models-framework/>

---

## Lattice URL (normative)

| URL | Status |
|---|---|
| `http://100.80.26.89:8080/v1` | **ONLY supported** Lattice entry for AFM (VPN / Tailscale tailnet) |
| `http://192.168.0.10:8080` | **UNSUPPORTED** |

Rationale recorded by CoS: the RPi4 frontend binds `127.0.0.1` + `100.80.26.89` only; nftables drops LAN `8080`. In this repository, bind semantics are deploy-time via `LATTICE_FRONTEND_BIND` (comma-separated `ip:port` pairs; loopback + tailnet — never a wildcard when the Pi is configured that way): see `cmd/lattice-frontend/main.go:437-457`. The concrete IPs and the nftables rule are **not** committed in tracked deploy files (placeholders by convention — `docs/README.md` Conventions); they are CoS / operator facts for this install.

S.E.E.D. and every other AFM-consuming client **must** use the supported URL above. Reaching the Mac AFM helper, `fm serve`, or any network-exposed AFM port directly is forbidden by the licence conditions.

---

## 1. Helper (Mac, on-device AFM)

**Role.** A Mac-local process that talks to Apple Foundation Models on-device and exposes an OpenAI-shaped chat surface **only** to the Lattice Gateway on the same host. The Gateway remains Go; AFM stays outside the Gateway process (same split as MLX — see `docs/specs/lattice-mlx-provider.md` §3 and `cmd/lattice-gateway/providers.go:251-254`).

**Binding (hard).**

- Listen on a **Unix domain socket mode `0600`**, or on **`127.0.0.1` only**.
- **Never** run Apple’s `fm serve` as the Lattice path.
- **Never** bind the helper (or `fm serve`) to a network / tailnet / LAN port (`0.0.0.0`, `100.x`, `192.168.x`, etc.).

**Concurrency (hard).** At most **one** in-flight AFM completion at a time (`concurrency 1`), matching both the licence condition and Lattice’s existing single local slot posture (`docs/specs/lattice-gateway.md` health `slots`, control local adequacy in `cmd/lattice-control/main.go:110-135`).

**Not in this doc’s enablement.** Shipping the helper binary, LaunchAgent/plist, or socket path is a later build/deploy stage. LAT-AFM-0 only records the contract.

---

## 2. Gateway files (future kind `"afm"`)

**Today (verified).** `LATTICE_GATEWAY_PROVIDERS` already drives a multi-kind registry. When set, `loadProviders` switches on `kind` and constructs providers for `ollama`, `mlx`, `mflux`/`sdxl`, and `speech`; unknown kinds error at startup:

```103:114:cmd/lattice-gateway/providers.go
		switch p.Kind {
		case "ollama":
			provs[p.Name] = &OllamaProvider{Endpoint: p.Endpoint}
		case "mlx":
			provs[p.Name] = &MLXProvider{Endpoint: p.Endpoint}
		case "mflux", "sdxl":
			provs[p.Name] = &MfluxProvider{Endpoint: p.Endpoint, name: p.Kind}
		case "speech":
			provs[p.Name] = &SpeechProvider{Endpoint: p.Endpoint}
		default:
			return nil, nil, fmt.Errorf("provider %q has unknown kind %q", p.Name, p.Kind)
		}
```

Empty `LATTICE_GATEWAY_PROVIDERS` keeps the legacy single-provider Ollama path (`cmd/lattice-gateway/providers.go:20-23`, `78-81`). Env wiring: `cmd/lattice-gateway/main.go:2180`.

**Future (this spec; not implemented).** A new compiled-in kind `"afm"`:

| piece | intent |
|---|---|
| `AFMProvider` in `cmd/lattice-gateway/providers.go` (or sibling) | HTTP(/socket) client to the **local-only** helper; chat-shaped like `MLXProvider` |
| `case "afm":` in `loadProviders` | select that implementation from JSON `kind` |
| model routes in the providers JSON | pin specific local model ids to `provider: "afm"` with optional `upstream` rename |
| `/health` announcement | list AFM-routed model ids under the afm provider name; do **not** claim embeddings/tool_calling unless implemented |

**Endpoint shape.** The Gateway’s `providers[].endpoint` for AFM must point at the helper’s **loopback or Unix-socket** URL — never a remote host. Fail loudly if misconfigured (same fail-loudly posture as MLX: `docs/specs/lattice-mlx-provider.md` §2.2).

**Out of scope for LAT-AFM-0.** No `.go` edits, no plist, no `gateway-providers.json` change.

---

## 3. Control routing + fallback

**Routing today (verified).** Control maps privacy / model locality / latency class to a gateway capability, then `selectGateway` filters on health, hosted model, and free slot (`docs/specs/lattice-control.md` §2; `cmd/lattice-control/main.go:110+`). There is **no** local→cloud fallback; `LOCAL_ONLY` fails closed (`docs/specs/lattice-control.md:45-49`, `docs/specs/lattice-failure.md`).

**AFM as a local model.** An AFM-served model id is a **local** model. Control learns it the same way it learns MLX-routed names: from the Gateway `/health` announcement (`models` / per-provider lists). Naming an AFM model under `LOCAL_ONLY` must never promote to cloud.

**Fallback (AFM-specific; still local-only).**

| situation | required behaviour |
|---|---|
| Kill switch OFF (§4) | Control / clients must not select AFM; traffic stays on existing local providers (Ollama / MLX) or fails closed per policy |
| AFM mapped but helper down | Fail loudly at the Gateway (do **not** silently re-home to cloud). Whether Control may re-resolve the **same request** to another *local* provider is a João decision (§ Open questions) — default proposal for build stage: **no silent remapping**; operator removes the AFM model route to fall back |
| AFM busy (concurrency 1 / slots) | Adequacy skip / `503` as for any local slot exhaustion — never cloud spill for `LOCAL_ONLY` |

**Provenance tag.** Every AFM-served completion that is retained or logged for S.E.E.D. / human review must carry a **provenance tag** identifying AFM as the engine (licence condition). Exact tag string / telemetry field is a João decision before enable.

---

## 4. Three-layer kill switch (default OFF)

AFM is **disabled by default**. All three layers must be ON for traffic to reach on-device AFM. Any single layer OFF is sufficient to keep AFM dark.

| layer | where | OFF means | ON means |
|---|---|---|---|
| **L1 — Helper** | Mac | helper not running / not listening on the allowed local socket or `127.0.0.1` | helper up, local-only bind, concurrency 1 |
| **L2 — Gateway registry** | Mac Gateway | no `kind: "afm"` entry (and no AFM model routes) in the live `LATTICE_GATEWAY_PROVIDERS` file; or Gateway build without `"afm"` case | `"afm"` kind registered + model routes present |
| **L3 — Control / client route** | RPi4 + S.E.E.D. | no capability alias / policy / S.E.E.D. route naming AFM models; S.E.E.D. not pointed at Lattice for AFM | explicit route to AFM model ids **only** through `http://100.80.26.89:8080/v1` |

**Default for LAT-AFM-0 and until §7 passes:** L1 OFF, L2 OFF, L3 OFF.

Flipping any layer is an operator act outside this docs PR. Docs must not instruct a one-shot enable.

---

## 5. S.E.E.D. side

- S.E.E.D. may consume AFM **only** by calling Lattice at **`http://100.80.26.89:8080/v1`** (licence condition 8 + URL table above).
- S.E.E.D. must **not** open the AFM helper, `fm serve`, or any Mac-local AFM port.
- **No portal or X-Wars use** of AFM outputs or of an AFM-backed Lattice route (licence condition 5).
- Outputs that might become long-term memory / CANONICAL require **human review before CANONICAL** and a **provenance tag** (licence conditions 6–7).
- **No AFM outputs** may be used to **train, fine-tune, distil or embed** any model (licence condition 4), including any S.E.E.D. embedding or distillation pipeline.

S.E.E.D. repo changes are out of scope for this lattice docs PR; this section is the lattice-facing contract those repos must honour when a later enable stage lands.

---

## 6. Docs list (follow-ups; not rewritten in LAT-AFM-0)

When a later **build** stage implements `"afm"`, update (do **not** do these in LAT-AFM-0 except the single stale-line fix called out in the task):

| doc | why |
|---|---|
| [`lattice-afm-provider.md`](lattice-afm-provider.md) (this file) | status → implemented / gated; link real helper path |
| [`lattice-apple-acceleration.md`](lattice-apple-acceleration.md) | already corrected for registry drift (§ P2); optional AFM cross-link later |
| [`lattice-mlx-provider.md`](lattice-mlx-provider.md) | kinds table grows `"afm"` |
| [`docs/manual/operations-manual.md`](../manual/operations-manual.md) | `LATTICE_GATEWAY_PROVIDERS` examples + AFM kill-switch ops |
| [`docs/handbook/architecture-handbook.md`](../handbook/architecture-handbook.md) | provider inventory / invariants |
| [`docs/guide/user-guide.md`](../guide/user-guide.md) | only if a client-facing AFM capability alias is introduced |
| [`docs/README.md`](../README.md) | specs index row for this file (optional indexing PR) |

LAT-AFM-0 **only** adds this spec and corrects the stale “hardcoded to ollama” line in `lattice-apple-acceleration.md`. No other existing doc is rewritten in that change.

---

## 7. Acceptance-before-enable

Before any operator flips L1/L2/L3 toward ON, **all** of the following must be true:

1. **Licence checklist signed off by João** — every verbatim condition in this file’s Licence section is acknowledged for the intended workload (personal non-commercial; no portal/X-Wars; no train/fine-tune/distil/embed; human review before CANONICAL; provenance tag).
2. **Helper bind audit** — listener is Unix socket `0600` or `127.0.0.1` only; confirmed **not** `fm serve` on a network port; `ss`/`lsof` evidence attached to the enable decision.
3. **Concurrency 1** — helper + Gateway local slot behaviour proven under a double-flight probe (second request queues or `503`s; never two concurrent AFM runs).
4. **URL path** — enable runbook uses **only** `http://100.80.26.89:8080/v1`; documents `http://192.168.0.10:8080` as UNSUPPORTED; S.E.E.D. config points at the supported URL.
5. **Kill switch rehearsal** — flipping any one of L1/L2/L3 OFF stops new AFM traffic without requiring the others.
6. **Code + config present** — Gateway build includes `kind: "afm"`; providers JSON and control/S.E.E.D. routes reviewed; still default OFF until this list is green.
7. **No merge-to-main enable** — enablement is an operator action on the live hosts, not a docs merge and not an unattended deploy from this spec.

Until then, AFM remains dark. LAT-AFM-0 is satisfied by the docs existing and the stale registry claim being corrected — **not** by AFM answering requests.

---

## Decision log

- **2026-11-20 (LAT-AFM-0):** Docs-only spec for future kind `"afm"`; licence conditions and supported Lattice URL recorded verbatim from CoS; three-layer kill switch default OFF; no code/config/deploy.
- **Registry correction (same change):** `lattice-apple-acceleration.md` P2 no longer claims the Gateway providers registry is hardcoded to `"ollama"` — that claim was already false once `LATTICE_GATEWAY_PROVIDERS` landed (`cmd/lattice-gateway/providers.go`).

## Open questions (João only — not decided here)

1. Exact helper implementation: custom Swift/Unix-socket helper vs any other local wrapper — provided it never uses networked `fm serve`?
2. On AFM mapped-but-down: keep fail-loudly (MLX posture) or allow Control to re-resolve to another **local** provider for the same request?
3. Provenance tag string / telemetry field name for AFM completions?
4. Capability alias name(s) for AFM (if any) vs literal model ids only?
5. Confirm Part C kill-switch layer names match L1/L2/L3 as written here (source file `AFM-LEGAL-AND-LATTICE.md` Part C was not readable on the cloud researcher host; conditions above are from the CoS task brief text).
