# Spec: The agent-facing surface

**Status:** Draft
**Scope:** `lattice-frontend`, `lattice-control`, gateway context ceiling
**Depends on:** [`inference-v1.md`](inference-v1.md), [`lattice-frontend.md`](lattice-frontend.md)

---

## 1. Purpose

Lattice was built for a *human* client: someone who types a capability alias and
does not care which model answers. An **agent runtime** is a different kind of
client, and it is not served well today. It:

- is configured with a **literal model string** (`hermes3:8b`), not a capability;
- needs **tool calling** to function at all (`tools` / `tool_calls`);
- **probes before it calls** — it checks `/v1/models` and `/health` to decide
  whether an endpoint is usable;
- does not send the `routing` envelope, because it has never heard of Lattice;
- keeps its own context budget and will not notice one imposed underneath it.

This spec defines the surface that lets such a client be pointed at
`:8080/v1` and work — while the control plane keeps deciding *what* and *where*.

**The design constraint is unchanged:** no new process, no new infrastructure,
and no second public contract. Everything here extends the frontend that already
owns the only public contract (see the
[handbook](../handbook/architecture-handbook.md) §9, invariant 1).

---

## 2. What is broken today

Verified against the running system on 2026-09-26.

| # | gap | failure mode |
|---|---|---|
| 1 | The frontend **rebuilds** the proxy body from `model`, `messages`, `stream` only | `tools`, `temperature`, `max_tokens`, `stop` are **silently dropped**. Streaming was lost this way once already (handbook §11). |
| 2 | An unrecognised `model` yields an **empty `model_name`** | A literal name fails obscurely, with no model in telemetry. |
| 3 | No `/v1/models`, no `/health` | A probing client gets 404 on all of them and concludes the API is absent — **this has already happened**: an external agent tested `/health` and `/` and reported that Lattice has no chat endpoint, which is false. |
| 4 | `routing.request_id` is required for correlation | A client that omits it produces blank-id telemetry lines. |
| 5 | Gateway caps context at `32768`; agent runtimes are configured for `65536` | Truncation the client cannot see. |

Gap 1 is the fatal one: an agent without `tools` cannot act.

---

## 3. Design

### 3.1 Body passthrough (a conformance fix, not a feature)

[`lattice-frontend.md`](lattice-frontend.md) §2 already specifies the correct
behaviour — *"update the `model` field in the JSON body"*. The implementation
instead builds a fresh body, which is exactly the drift the handbook §11 gotcha
records. This change is therefore a **return to spec**:

- Clone the client's request body.
- Replace `model` with the resolved `model_name`.
- Remove any client-supplied `routing`, then re-inject it **only** for the local
  path. Cloud endpoints speak plain OpenAI and reject the envelope.
- Every other field passes through untouched.

The default inverts from *deny unless copied* to *allow unless rewritten*, which
is the correct default for a proxy and removes the failure class rather than the
instance.

> **Rule.** The frontend may not enumerate the fields a client is allowed to
> send. Enumerating is what broke it.

### 3.2 Model resolution: alias or literal

The `model` field resolves in one of two modes:

| input | meaning | behaviour |
|---|---|---|
| a **capability alias** (`local-brain`, `local-coder`) | "policy picks the model" | resolved per target, exactly as today |
| **anything else** | "run *this* model" | passed through **verbatim** as `model_name` |

**The guard: `model_name` is never empty.** If the client named something, that
name is what reaches the target, and the target's own error is the report. On
the local path an unknown model produces Ollama's `404`, which the gateway
already surfaces as a `500` carrying the model id; telemetry then shows the name
that failed. The bug was never that a bad name fails — it is that it failed
*anonymously*.

> **Why no name→capability table.** It would have to be maintained, would lose
> the client's exact model identity, and would break the moment either side
> renames a model. Passthrough plus a real error is smaller and truer.

**Optional early rejection.** Control may pre-empt a *class* mismatch it can
identify with certainty — a `:cloud`-suffixed literal whose decided target is
the local gateway, for instance — with a `400` naming the model and the target.
This is a convenience, not the safety mechanism; the safety mechanism is that
the name is never lost.

### 3.3 Discovery surface

Two read-only endpoints, served by the frontend:

- **`GET /v1/models`** — the client-facing namespace: the capability aliases,
  each with a `context_length` reflecting the real ceiling (§3.5). This is the
  namespace clients are *expected* to use, so it is the namespace it advertises.
- **`GET /health`** — frontend liveness. Cheap, and the natural probe target.

> **This is the highest-leverage change in the spec.** The external misread was
> a *discovery* failure: a single-route service is indistinguishable from a
> non-existent one to any client that probes first, and most do.

### 3.4 Correlation without client cooperation

If the client supplies no `routing.request_id`, the frontend generates one and
returns it in an **`X-Request-Id`** response header. The three telemetry streams
stay keyed as they are; this only ensures the key is never blank, so a run is
always identifiable on the Bee screen.

> **Ordering constraint.** The id must be set on the request **before** the
> control call, not just before the proxy call: the frontend marshals the typed
> request to `/route`, so an id assigned later is one control never logged, and
> the run is again correlatable in two planes out of three.

### 3.5 Context ceiling

The gateway ceiling rises to **`65536`** to match what agent runtimes carry.

This is a memory decision, not a formatting one: a larger ceiling means a larger
KV cache, and the Mac's SSD is the thing the margin exists to protect
(handbook §7). The change is accepted with that understood, and it is the one
item here that needs a **measurement** before it is trusted — see §6.

The ceiling must also appear in `/v1/models`, so the limit is discoverable
rather than invisible.

---

## 4. Non-goals

- **`/v1/embeddings`.** Deferred. Embeddings carry no routing decision, and the
  consumer that prompted this work already has a working embedding path.
- **A separate shim process.** Explicitly rejected: it would be a second public
  contract, drift from the frontend, and duplicate a route that already exists.
- **A model inventory in control.** That is infrastructure, and invariant 5
  forbids it.
- **`LOCAL_PREFERRED` fallback semantics.** [`inference-v1.md`](inference-v1.md)
  §2 documents a `LOCAL_PREFERRED` level ("prefer Mac, fall cloud back") that
  the policy table has never implemented. Out of scope here — but the drift is
  real and is recorded in §7 so it is not rediscovered.

---

## 5. Invariant check

| invariant | status |
|---|---|
| 1 — one public entry point | **held** — extended, not duplicated |
| 2 — the Mac computes, the Pi routes | held |
| 3 — `LOCAL_ONLY` never falls back | held, untouched |
| 4 — the gateway translates, it does not decide | held — no routing moves into it |
| 5 — no new infrastructure | **held** — no new process, no inventory, no bus |
| 6 — Bee is not modified | held |
| 7 — protect the Mac's SSD | **at risk** — the 65536 ceiling raises KV-cache size; measured in §6 |
| 8 — no usernames/hostnames/addresses | held |
| 9 — telemetry shares one key | **strengthened** — the key can no longer be blank |

---

## 6. Acceptance

Automated (`lattice-frontend`):

1. A body carrying `tools`, `temperature`, and `max_tokens` reaches the target
   with all three intact — the regression that started this.
2. `model: "local-coder"` resolves per target; `model: "hermes3:8b"` passes
   through verbatim; an unknown name yields a **non-empty** `model_name`.
3. `routing` is forwarded on the local path and **absent** on the cloud path.
4. `GET /v1/models` lists the aliases with a `context_length`; `GET /health`
   returns `200`.
5. A request with no `routing.request_id` returns an `X-Request-Id` header whose
   value appears in all three telemetry streams.

Manual, on the pair:

6. A tool-calling request completes end-to-end against a local model.
7. **Memory measurement.** With the ceiling at `65536`, one local inference must
   leave `Swapouts` flat (`vm_stat | grep -i swap`). If it does not, the ceiling
   is wrong and comes back down — this is the acceptance test for §3.5, and it
   outranks the convenience that motivated it.

---

## 7. Reconciliation and recorded drift

- **A parallel shim may already be under construction.** An external agent
  platform is independently specifying a translation shim for Lattice, on the
  premise (established above as false) that Lattice has no chat endpoint. It is
  being allowed to land. When it does, its spec should be diffed against this
  one and anything it got right folded in rather than discarded.
- **Recorded drift:** `LOCAL_PREFERRED` (§4). Spec promises it; the policy table
  does not implement it.
