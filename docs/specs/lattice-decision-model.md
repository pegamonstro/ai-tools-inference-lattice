# Spec: A decision model as a future operator of model handles

**Status:** Investigated — not scheduled. Reserved future axis.
**Host:** Mac (Gateway) — a decision model is local inference, so it can only live there.
**Gate:** none (no implementation planned).

This is a feasibility investigation, not an implementation target. It asks one
question: could a learned **"System 1" decision model** (Laya, or a hosted peer
like Jev) replace or augment the deterministic logic that currently operates
Lattice's **model handles** — the alias→model resolution and the capability-based
routing of [`lattice-gateway-registry.md`](lattice-gateway-registry.md)? The
answer is *conditionally yes, not now*, and this document records why, what the
trigger conditions are, and the integration shape if they ever fire.

## 1. What "model handles" means here

Lattice's model registry is two resolutions deep (registry spec §3.5):

1. **Alias → concrete model** (`local-brain` → `granite3-moe:3b` local /
   `gemma4:31b-cloud` cloud). This is **policy**, and it lives in Control Plane
   config (`capabilities` map in `cmd/lattice-control/main.go`). "RPi4 decides."
2. **Concrete model → gateway** (`granite3-moe:3b` → which healthy gateway hosts
   it, with capacity). This is **fact**, announced by the gateway on `/health`.
   "Mac (and cloud) report."

"Operating model handles" means the decisions made over those two resolutions,
plus the adequacy routing that sits beneath them (`selectGateway`, filter →
score). Today every one of those decisions is a deterministic rule:

- `requiredCapability(privacy, latency, model)` — hard rules (cloud model?
  `LOCAL_ONLY`? `interactive`?) → `local` / `cloud`.
- `resolveModel` — alias lookup, or literal pass-through.
- `selectGateway` — filter (capability, health, hosts-model, slots), then score
  (cloud → cheapest; local → first match).

This is the thing a decision model would be evaluated against.

## 2. What Laya is (and is not)

Laya ([HF](https://huggingface.co/convaiinnovations/laya),
[announcement](https://laya.convaiinnovations.com/)) is a **non-autoregressive
decision model**: a bidirectional ModernBERT-large backbone (~395 M) plus a
decision head trained from scratch (2 transformer layers + an option-marker
scorer + an act/escalate head), ~421 M total. It does not generate text. It
scores a fixed set of options in one forward pass and returns a **calibrated
probability distribution**. Three primitives:

- `choice` — pick one of up to ~20 named options (degrades past that),
- `score` — ordinal rubric level (weakest primitive),
- `noul` — calibrated boolean probability.

Training is **RLCD** (Reinforcement Learning for Calibrated Decisions): REINFORCE
with a group-mean baseline, rewarded by a strictly proper scoring rule (log /
Brier) so the model is incentivized to report *honest* uncertainty, not just the
right label.

Deployment facts that shape the integration question:

- **No GGUF.** Laya's encoder has not been ported to llama.cpp, so it cannot
  ride the Ollama path. It is not a drop-in Ollama model.
- **ONNX Runtime is the real path.** Community exports
  ([inferenceprince/laya-onnx](https://huggingface.co/inferenceprince/laya-onnx),
  [receptron/laya-onnx](https://huggingface.co/receptron/laya-onnx),
  [mariojcr/laya-onnx](https://huggingface.co/mariojcr/laya-onnx)) run without
  PyTorch. fp16 weights are ~843 MB; cold start 3–5 s (vs 25–35 s for PyTorch);
  ~33–40 ms/forward pass on GPU, ~370 ms for a batch of 4 on 20 CPU threads.
- **The graph returns raw logits.** You must divide by the fitted temperature
  (from `rl_agent_config.json`) before softmax to get *calibrated* probabilities.
- **Ships over-confident** and the English checkpoint collapses on non-Latin
  script *while staying confident*, so confidence-gating alone won't catch
  out-of-domain input.

## 3. The reframing that decides this

Lattice's routing is **already a System 1 fast path.** It is deterministic,
runs in nanoseconds, is correct-by-construction, and fails loudly. A decision
model is *also* a System 1 fast path — so it would not be replacing a slow
generative judgment call (which would be an obvious win). It would be replacing
a **fast deterministic rule**. That strips the usual justification ("it's
faster than an LLM") and leaves only one: **expressiveness of judgment** — the
ability to make routing decisions that cannot be cleanly written as rules.

## 4. Where a decision model could earn its place

Three places in the model-handle path involve judgment rather than policy or
fact:

1. **Request classification when the client declared no intent.** An
   OpenAI-SDK agent names a model and sends no routing envelope; `isCloudModel`
   can only read the tag, and otherwise Lattice defaults to local. A decision
   model could classify the *request* ("this is a coding task" → `local-coder`)
   and select the handle without the client declaring it. This is the cleanest
   fit: a `choice` over the existing, small alias set — low cardinality, exactly
   Laya's strength.
2. **Semantic adequacy scoring.** `selectGateway` filters on *declared*
   capability and scores cloud on cost alone. It cannot answer "is this model
   actually good at *this* request?" — a judgment a calibrated model could add.
   This is the same axis that would later let `vision` / `reasoning` capability
   bits be *classified* rather than *declared*.
3. **Capability taxonomy expansion.** As the modality axis grows, deciding
   *which capability a request needs* becomes a classification problem over a
   small open set.

## 5. Where it must never go

- **Policy stays deterministic.** Alias → concrete model is Control Plane
  config because it is *policy* ("RPi4 decides"). A decision model may
  *suggest* a handle, never own the mapping. Any integration keeps the config
  map authoritative.
- **The `LOCAL_ONLY` safety gate stays a rule.** The entire point of
  `requiredCapability` / `selectGateway` is that a local-only request is *never*
  silently promoted to cloud, and an irreconcilable request *fails loudly and
  predictably*. A probabilistic model introduces exactly the nondeterminism that
  constraint forbids. A decision model can only be **advisory**, feeding a
  suggestion into the existing deterministic gate — never replacing it.

## 6. Costs and blockers (why "not now")

- **A second resident model on a memory-pressured host.** The Mac is 16 GB, and
  the current context-ceiling work is precisely about not exhausting it. The
  gateway's `slots: 1` design encodes "one resident local model at a time." A
  decision model is either a *second* resident model (memory contention with the
  generative model it is routing toward) or must be loaded/unloaded around it
  (adds the 3–5 s ONNX cold start to the request path). Neither is free.
- **Go-stdlib-only is preserved only via a sidecar.** Laya is Python (native) or
  ONNX Runtime (community). ONNX Runtime has Go bindings but needs cgo and a
  native library, which violates "Go stdlib only" *for whichever binary hosts
  it*. The accepted pattern is a **sidecar** — a small HTTP `POST /decide`
  process, exactly as MLX and mflux already are. That is *not* "new
  infrastructure" (no Redis/Kafka/DB), but it is a new runtime with a new
  dependency chain.
- **Fine-tuning burden + the "local models never verify" rule.** Laya is
  near-chance zero-shot; it needs labeled routing decisions to be useful, and
  per-domain temperature fitting to be calibrated. Per the build-orchestration
  rule, the supervision signal for that loop cannot come from local inference —
  it needs a non-local source of truth, which is a real cost to stand up.
- **Calibration is fragile.** Over-confident out of the box; non-Latin collapse
  undetected by confidence-gating. Any adoption needs a calibration harness
  before it touches a routing decision.

## 7. The integration shape, if it ever fires

If the trigger conditions (§8) are met, the shape is:

1. A **`lattice-decide` sidecar** on the Mac, serving Laya via ONNX Runtime over
   a minimal `POST /decide` (state + `choice`/`noul` questions → calibrated
   probabilities). No GGUF, no Ollama involvement.
2. The Control Plane's routing gains an **optional advisory call** — used only
   for the judgment cases in §4 (request classification / semantic adequacy),
   never for policy or the `LOCAL_ONLY` gate. The deterministic filter → score
   remains the authority; the model's output is a *suggestion* it can accept,
   ignore, or use to rank.
3. A **calibration harness** (labeled routing decisions + temperature fitting)
   that runs against a non-local source of truth, per the "local models never
   verify" rule.

## 8. Trigger conditions

Reserve this axis. Build it only when **all** of:

- routing decisions actually need request-*content* classification (a client
  that doesn't declare intent, or a modality taxonomy rules can't express), and
- the Mac has RAM headroom for a second resident model (or the cold-start cost
  is acceptable), and
- a labeled routing-decision set exists to fine-tune and calibrate against.

Until then the deterministic router is correct, cheap, and loud — a learned
decision model adds expressiveness it does not yet need.

## 9. Decision log

- **2026-09-30 — reserve, do not build.** The deterministic model-handle
  routing is already a fast System 1 path, so a decision model's only value is
  *judgment*, and current routing needs judgment only in the undeclared-intent
  niche. Source: the §3 reframing.
- **2026-09-30 — ONNX sidecar, not Ollama.** Laya has no GGUF; the accepted
  integration is a `lattice-decide` sidecar over ONNX Runtime, mirroring the
  MLX/mflux pattern, keeping the Go-stdlib core intact.
- **2026-09-30 — advisory only.** A decision model may suggest a handle or rank
  adequacy, never own policy or the `LOCAL_ONLY` safety gate.
