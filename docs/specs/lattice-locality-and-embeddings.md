# Spec: Reported locality and the local embeddings route

**Status:** Draft
**Scope:** `lattice-control`, `lattice-frontend`, `lattice-gateway`
**Depends on:** [lattice-observability.md](lattice-observability.md),
[lattice-agent-surface.md](lattice-agent-surface.md),
[lattice-gateway.md](lattice-gateway.md), [inference-v1.md](inference-v1.md)
**Date:** 2026-09-26

Two changes, commissioned together because one consumer needs both. They close
the two open questions [lattice-build-orchestration.md](lattice-build-orchestration.md)
§5.1 and §9.3 left standing.

1. **`locality` becomes a reported attribute** instead of a class every tool
   re-derives by matching two literal target names.
2. **`/v1/embeddings` is served** through the frontend and the gateway, so an
   agent runtime on the RPi4 has a working embedding path.

Both are *exposure* changes, not policy changes. Lattice already knows which
class a target belongs to, and Ollama already embeds. Neither fact currently
reaches a client.

---

## 1. Why now

- **Observability §2 already requires it.** "Routing distribution (% cloud vs
  % local)" is a specified output of `lattice-stats`. There is no way to produce
  it today except by matching the literals `mac-gateway` and
  `ollama-cloud-secondary` — the requirement is on the books and unmet.
- **The orchestrator needs memory.** Its units are dispatched as separate
  processes with no shared conversation; recall is the only continuity it has.
  Recall rides on embeddings, and embeddings currently return `404`.
- **Every new consumer pays the name-matching tax again.** The frontend already
  carries it (§2), and a stats rewrite would need a second copy of it. The
  registry the rule comes from lives in control, so no other layer can derive
  the answer without either reaching for that registry or hardcoding the names.

## 2. What is true today (verified against the code, 2026-09-26)

| fact | where |
|---|---|
| Locality is encoded **structurally**: `providers` map → cloud, `gateways` map → local, and each entry carries `Capabilities: ["cloud"｜"local"]` | `cmd/lattice-control/main.go` registries |
| The frontend **literal-matches** a target name to decide whether to inject the local routing envelope | `cmd/lattice-frontend/main.go` `buildProxyBody` |
| Control's `Decision` carries `target`, `endpoint`, `model_name` — **no class** | `cmd/lattice-control/main.go` |
| Three telemetry streams; **only two carry `target`**; none carries a class | control, frontend, gateway telemetry |
| The gateway stream **does** carry `prompt_tokens` / `completion_tokens` | `cmd/lattice-gateway/main.go:89-90` |
| Control relays the gateway's events **verbatim** (`[]json.RawMessage`) into the Pi's local stream | `cmd/lattice-control/main.go:263` |
| `/v1/embeddings` returns `404` through the frontend | probed 2026-09-26 |
| Control's `/route` **never reads `messages`** — it routes on `model`, `routing.privacy`, `routing.latency_class` — so a body of any shape routes correctly | `cmd/lattice-control/main.go` `handleRoute` |
| The cloud path echoes back **the same `Decision` struct** through the dispatcher, so a field set before dispatch survives | `cmd/lattice-control/main.go:437` |

The first row is the whole argument for §3: the class is not missing, it is
**unreported**.

## 3. Locality as a reported attribute

### 3.1 The attribute

| property | value |
|---|---|
| name | `locality` |
| values | `local` \| `cloud` \| `unknown` |
| rule | the **registry the target was chosen from** — `gateways` → `local`, `providers` → `cloud` |
| default | `unknown` |

The values are deliberately the **same strings already in `Capabilities`**.
One vocabulary, not two: an entry that advertises `"cloud"` produces
`locality: "cloud"`, and no translation table exists to drift.

`unknown` covers the refusal paths, where `target` is `""` — a `LOCAL_ONLY`
request carrying a cloud-tagged model, an unhealthy local gateway, or a
pre-decision failure. A refusal is the event the routing policy exists to
produce, so it is bucketed rather than omitted.

This is a **Lattice-specific attribute**. It is not a `gen_ai.*` convention and
must not be mapped onto `deployment.environment.name`, which describes a
deployment tier (development/production), not where a request was executed.

### 3.2 Where it is emitted

| producer | stream / payload | change |
|---|---|---|
| control | `Decision` | + `locality` |
| control | `telemetry-control.jsonl` | + `locality` |
| frontend | `telemetry-frontend.jsonl` | + `locality`, taken from the decision |
| gateway | `telemetry-gateway.jsonl` | + `locality: "local"` (constant) |
| `lattice-stats` *(not in this plan)* | — | once rewritten, reads `locality` for §2's split |

The gateway's entry is constant because the gateway **is** the local executor.
It has no `target` field today and does not gain one: its provenance is the
gateway, and reporting `locality: local` gives the three streams one shared
dimension without inventing a target it does not have.

### 3.3 Why a field *beside* the name, never instead of it

`target` stays. It is the source of truth and it is still what §2's first
requirement — *average latency per target* — is keyed on. `locality` is a
derived attribute sitting next to it.

Duplicating a derived value beside its source is the right call **only** when
one producer writes both atomically and the result is rebuildable. Control
satisfies both: it writes `target` and `locality` on the same line from the
same decision, and either can be recomputed from the other plus the registry.
The failure mode this normally invites — projection drift — needs two writers,
and there is one.

### 3.4 Why consumers must not derive it themselves

Derivation requires the registry, which lives in control. Every consumer that
derives it therefore either reaches for the registry (coupling, and a second
copy of the rule) or hardcodes the names (today's defect). The current code
demonstrates exactly this: the frontend already carries a literal match, and
`lattice-stats` would need a second one.

Encoding meaning in an identifier and parsing it back out is what the
conventions this research drew on uniformly reject: identifying attributes are
meant to be minimally sufficient and stable, with semantics in separate
attributes — Prometheus states the same rule as "do not put the label names in
the metric name", and Google SRE's alerting chapter replaced parsing custom
output with structured key/value series for the same reason.

### 3.5 `unknown`, and what happens to a value nobody recognises

Enums are open by definition: **adding** a value is non-breaking, changing or
removing one is not. So consumers must:

- treat `locality` as a closed set of **known** values plus everything else;
- funnel an unrecognised value into `unknown` rather than dropping the event;
- always retain `target`, which is never dropped.

A future `edge` or `partner` locality then degrades to "not cloud, not local"
instead of silently vanishing from a count — which is the failure that makes
an enumeration dangerous in the first place.

### 3.6 What it retires

| literal | replaced by |
|---|---|
| `decision.Target == "mac-gateway"` in the frontend | `decision.Locality == "local"` |

## 4. The local embeddings route

### 4.1 Why embeddings are local-only

Not a policy choice — a property of the platform. Ollama's cloud passthrough
**refuses** embeddings, and no `-cloud` embedding tag exists. Control already
resolves an untagged embedding model to the `local` capability, so **no routing
logic changes**; the routing was always correct, only the route was missing.

This also means the route inherits the existing privacy posture rather than
weakening it: repo content and briefs are embedded on the Mac, never sent to a
cloud endpoint to be embedded.

### 4.2 Frontend

A `/v1/embeddings` route sharing the existing *parse → control → proxy* flow
with chat. Two things differ, and both are already parameters of the flow:

- **The upstream path.** Today the frontend forces `/v1/chat/completions`;
  the forced path becomes route-dependent.
- **The routing envelope.** It is injected only on the chat path, where it is
  the local translation layer's contract. An embeddings body has no such
  contract, so it is forwarded without one.

The body forward **stays non-enumerating**. It forwards the client's own body
and rewrites only the resolved model name, which is exactly why an unfamiliar
body shape is safe: the earlier version that rebuilt the body from
`model`/`messages`/`stream` silently dropped `tools`, and an agent lost the
ability to call anything.

### 4.3 Gateway

A `/v1/embeddings` route that forwards to **Ollama's own `/v1/embeddings`**.
Ollama implements that endpoint natively, so nothing is translated — the
response's shape, its base64 encoding and its precision are Ollama's by
construction, which is the reliable option precisely because it adds no code
that could diverge.

The alternative, translating to the native `/api/embed` and rebuilding the
OpenAI response, is what the third-party proxies in the research do; it is
strictly more code and more places to be wrong. It is not needed here because
the gateway's target *is* Ollama.

The gateway does **not** reuse the chat handler. That handler decodes into the
chat `Request` shape and would have to be generalised; the embeddings route is
a separate handler that reads the body, takes the slot, forwards, and writes
one telemetry line.

### 4.4 Concurrency: the embedding takes the single inference slot

An embedding **shares the gateway's one inference slot**. This is not a
conservative default; it is the same protection the slot already provides for
chat. With one model resident, a concurrent embed evicts the resident chat
model — which is the swap pressure this project has already paid for once.

The memory-budgeter check is **kept**. With the slot held, the check is no
longer a concurrency guard but a floor on the machine being usable at all, and
a `429` naming memory pressure is a visible, correctable failure where an
out-of-memory event is not. This is the project's existing preference — fail
loudly — applied to a new route. The research flagged the opposite risk: a
1.5 GiB margin rejecting a small embed while a chat model holds RAM. That risk
is real, and it is why §9.2 tracks it against telemetry rather than deciding it
in advance.

### 4.5 What is deliberately not reused

Embeddings must **not** go through the chat path's context machinery —
`contextWindow`, `num_ctx`, `resolveMaxTokens`, `kv_cache_type`. Those are
*generation* options: an embedding model has a fixed, small context and no
output to predict. Passing them is at best ignored and at worst an error.

### 4.6 Wire notes worth recording

- base64 embeddings are **raw float32, little-endian**, standard encoding.
- Input as a **token array returns `400`** — Ollama diverges from OpenAI there.
  Clients sending token arrays must instead send text.
- an **empty `encoding_format`** (`""`) is rejected rather than treated as a
  default. The third-party proxies in the research strip the key when it is
  empty; **Lattice does not**, because it forwards the body rather than
  rebuilding it. A client that sends `""` therefore gets Ollama's rejection —
  which is the correct outcome for a body Lattice was not asked to sanitise,
  and worth recording so nobody later assumes a filter exists.

### 4.7 The tradeoff, stated honestly

The Mac keeps **one model resident**. Interleaving an embedding call with a
chat turn evicts the chat model, and the next turn reloads it. In a loop whose
turns run ~220 s, recall could end up paying a reload per interleave.

This is accepted rather than hidden, for three reasons: the alternative
(cloud embedding) sends the content being embedded to a cloud endpoint, which
is the wrong direction for this project; the alternative of disabling memory
leaves the orchestrator amnesiac between units; and the cost is bounded by the
slot, so it is a *serial* cost rather than concurrent load. §9.2 required it to
be **measured** rather than assumed. Measured on 2026-09-26 against
`granite4:3b` with `embeddinggemma:latest` on the Mac: a warm chat turn took
**0.10 s** (median of five, all with identical one-token output), the embedding
**1.23 s**, and the same chat turn immediately after the embedding **1.54 s** —
a reload penalty of **1.44 s**. The five warm turns — the 2.96 s first call
carried the model load and is excluded — all fell between 0.08 s and 0.12 s,
including the two taken after the second interleave, so the penalty is the
interleave and not drift.

Two things that figure settles, both against the guess it replaces. The reload
does **not** hide behind the prefill: at roughly 14× a warm turn it dominates a
short turn rather than being absorbed by it. And it is that small largely
because the gateway keeps the context small: `contextWindow` sizes `num_ctx` to
the prompt, starting at 2048 and doubling until the prompt fits, with the KV
cache quantized to q8_0. This one-token workload got 2048 — the value the
telemetry line records — not the 32768 ceiling. The same reload at Ollama's
default context measured 8.8 s and grew residency to 12.7 GB of this 16 GB host,
enough to trip the memory margin and make the gateway report unhealthy. The
acceptance above therefore stands against the ~220 s turns it is written for,
where 1.4 s is under 1% of one turn — not because prefill absorbs the reload.

## 5. Corrections to the record

### 5.1 The token claim was overstated

[lattice-build-orchestration.md](lattice-build-orchestration.md) §5.2 concludes
that token accounting is "unmeasurable". The scoped half is true — neither
`telemetry-control.jsonl` nor `telemetry-frontend.jsonl` carries token fields.
The conclusion is not: **the gateway emits both** (§2) and control relays them
onto the Pi verbatim.

So **local** token spend is measurable today. What is genuinely absent is
**cloud** token spend: the frontend reverse-proxies the response untouched and
never decodes its `usage`. That is precisely the quantity
[lattice-observability.md](lattice-observability.md) §2 asks for ("Total token
spend (Cloud)"), and it is now named there as an open gap rather than as a
general absence.

The prototype's finding survives the correction — `lattice-stats` cannot
produce cloud spend from the streams it reads — but the recorded *reason* was
wrong and is fixed.

### 5.2 The embeddings non-goal is retracted

[lattice-agent-surface.md](lattice-agent-surface.md) §4 deferred
`/v1/embeddings`, on the grounds that "the consumer that prompted this work
already has a working embedding path". That consumer's path **was the bypass**
— pointing directly at the Mac's Ollama — and the bypass was deliberately
closed when both Hermes providers were repointed at the frontend. The premise
is gone, so the non-goal goes with it.

### 5.3 The two literal names are retired

`mac-gateway` and `ollama-cloud-secondary` stop being vocabulary that tools
must know. They remain values of `target`, which is data.

## 6. Non-goals

- **No cloud embedding route.** Platform-limited (§4.1), and the privacy
  direction is wrong.
- **No embedding cache, batch queue, or second process.** Anti-drift rule 5.
  The route is a passthrough; state stays files.
- **No change to routing policy.** `LOCAL_ONLY` and the cloud-locality tag rule
  are unchanged.
- **No new telemetry store.** `locality` is a field on the existing three
  streams.
- **Cloud token accounting is named, not delivered.** §5.1 records it; it is
  not this spec's work.

## 7. Invariant check

Against [lattice-design.md](../lattice-design.md) §6:

| rule | status |
|---|---|
| 2. RPi4 decides, the Mac executes | **held** — embeddings are decided in control and executed on the Mac |
| 5. No speculative infrastructure | **held** — §6; no new component, no new store |
| 6. No swap thrashing | **held** — §4.4; the one slot is the protection, and it is extended to the new route rather than bypassed |
| 7. Deterministic infrastructure | **held** — a passthrough route and an additive enum field |
| 8. Every phase has an exit test | **held** — §8 lists the acceptance checks |

## 8. Amendments required elsewhere

Named so they cannot drift. Each is a consequence of §3 or §4.

| document | change |
|---|---|
| [specs/lattice-agent-surface.md](lattice-agent-surface.md) §4 | retract the `/v1/embeddings` non-goal (§5.2) |
| [specs/lattice-build-orchestration.md](lattice-build-orchestration.md) §5.1, §9.1 | embeddings resolved, not open |
| [specs/lattice-build-orchestration.md](lattice-build-orchestration.md) §5.2 | correct the token claim (§5.1) |
| [specs/lattice-build-orchestration.md](lattice-build-orchestration.md) §9.3 | closed by §3 |
| [specs/lattice-observability.md](lattice-observability.md) §2 | the cloud/local split reads `locality`; the cloud-token gap is named |
| [specs/lattice-control.md](lattice-control.md) | `Decision` and the control schema gain `locality` |
| [specs/lattice-frontend.md](lattice-frontend.md) | the new route; the forced upstream path becomes route-dependent |
| [specs/lattice-gateway.md](lattice-gateway.md) | the new route; the gateway schema gains `locality` |
| [guide/user-guide.md](../guide/user-guide.md) | document `/v1/embeddings` and its limits |
| [handbook/architecture-handbook.md](../handbook/architecture-handbook.md) | the telemetry schema and the new attribute |
| [README.md](../README.md) | index this spec |

## 9. Open questions

1. **Should `locality` reach the client?** Today it is telemetry-only. It could
   also appear in the chat response for a client that wants to know where its
   request ran. Not needed by any current consumer.
2. ~~The embedding reload cost.~~ **Measured 2026-09-26 — see §4.7.** The
   mitigation, if one is wanted, is on the caller's side (batching, or fewer
   recall calls); it is not an unmeasured policy change here.
3. **Does the budgeter reject small embeds?** §4.4 keeps the check and accepts
   the risk. If `429`s appear against embeddings in telemetry, the exemption
   the research suggested becomes the fix.
4. **Cloud token spend** (§5.1). Which layer should decode `usage` — the
   frontend, which sees the whole response, or the gateway, which sees it
   first? Unchanged from build-orchestration §9.2, and still open.
5. **The semantic defect the prototype found.** Pooling control's
   `decision_time_s` with the frontend's `total_time_s` into one "average
   latency per target" produces a figure true of neither layer. `locality` does
   not fix it; a `lattice-stats` rewrite must.
