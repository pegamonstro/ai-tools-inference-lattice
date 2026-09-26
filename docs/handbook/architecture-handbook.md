# Lattice Architecture Handbook

Why Lattice is built the way it is. This is the document to read before
changing it — it records the invariants that must not drift, the reasoning
behind them, and the traps that have already bitten once.

For step-by-step operation see the [Operations Manual](../manual/operations-manual.md).
For client usage see the [User Guide](../guide/user-guide.md).

---

## 1. What Lattice is

A thin control layer over inference that already exists. It answers two
questions and nothing else:

- **What** capability does this request need?
- **Where** is it allowed and best to run?

Everything else — the model server, the transport, the API — is someone else's
job. That restraint is deliberate: Lattice is a *router*, not a platform.

> **No speculative infrastructure.** No Kubernetes, Redis, Kafka, RabbitMQ, or
> Postgres. If a routing decision needs a message bus, the design is wrong.

---

## 2. Frozen topology

Three planes, each with exactly one job:

```
        client
          │  OpenAI Chat Completions + routing envelope
          ▼
   ┌──────────────┐   "what + where"
   │   FRONTEND   │   :8080   single entry point
   └──────┬───────┘
          │  POST /route  →  {target, endpoint, model_name}
          ▼
   ┌──────────────┐   the brain: capability map, health, policy
   │   CONTROL    │   :8082
   └──────┬───────┘
          │  proxy to the decided endpoint
          ▼
   ┌──────────────┐   "how"   (Mac only)
   │   GATEWAY    │   :8081   OpenAI → Ollama translation, memory guard
   └──────┬───────┘
          ▼
      Ollama (local models)          ── or ──    Ollama cloud (external)
```

The control plane **decides**; the gateway **executes**. The frontend owns the
only public contract. These bounds are load-bearing — do not let routing logic
leak into the gateway, or Ollama translation leak into the frontend.

---

## 3. Hardware tiers (verified)

| host | class | OS | role |
|---|---|---|---|
| RPi4 | control plane | **Debian 13 (trixie)**, arm64 | frontend + control + feeder |
| Mac | gateway | macOS, Apple silicon | gateway + local Ollama |
| external | cloud | — | cloud models via Ollama |

**The Mac is the only host permitted to run local models.** The RPi4 is
cloud-only: its hardware cannot sustain local inference, and asking it to try
is a category error. A local request arriving at the Pi is *forwarded to the
Mac* — never computed locally.

> The RPi3 is **not part of this system**. Lattice is strictly the RPi4 and the
> Mac. (An earlier design placed a security appliance on the RPi3; it is out of
> scope and its spec exists only as a record.)

---

## 4. The capability model

The client's `model` field is **not** a model name — it is a capability alias
resolved per target:

```go
"local-brain": {local: "granite4:3b", cloud: "gemma4:31b-cloud"},
"local-coder": {local: "hermes3:8b", cloud: "deepseek-v4-pro:cloud"},
```

This is the single most important interface decision in the system: it lets a
client ask for *"a coding model, run it wherever policy allows"* without knowing
anything about what hardware exists. Adding a capability is a one-line map
entry; adding a model behind it is an operator concern.

---

## 5. Request lifecycle

1. **Client → frontend.** OpenAI-shaped body, optionally carrying `routing`.
   The frontend parses the body and does not inspect the messages.
2. **Frontend → control.** The frontend posts the whole request to `/route`
   (5-second timeout). The control plane holds the capability map and the
   gateway health state; it returns `{target, endpoint, model_name}` — or an
   error status, which the frontend **propagates verbatim** rather than masking
   as a 500.
3. **Frontend rebuilds the body.** A fresh `proxyBody` is constructed with the
   resolved model name, the messages, and `stream`. For the local path only,
   the `routing` envelope (`request_id` + `provider_params`) is forwarded —
   cloud endpoints speak plain OpenAI and would reject it.
4. **Frontend → target.** A `httputil.ReverseProxy` forwards to the decided
   endpoint. For streaming requests the proxy flushes SSE frames as they arrive.
5. **Gateway → Ollama.** On the local path the gateway translates the OpenAI
   body into Ollama's options (`num_ctx`, `num_predict`, `kv_cache_type`),
   acquires the single inference slot, and calls Ollama.
6. **Telemetry.** Each plane appends one JSONL event, all sharing `request_id`.

---

## 6. Routing policy

The decision is a small, explicit table — not a scoring function:

| `privacy` | `latency_class` | target |
|---|---|---|
| `LOCAL_ONLY` | any | local gateway, or **503** — never cloud |
| anything else | `interactive` | cloud |
| anything else | `batch` | local gateway |

**`LOCAL_ONLY` is a hard gate, not a preference.** If no healthy local gateway
exists the request *fails*. It is never silently rerouted to cloud. This is the
sovereignty guarantee, and it is the behaviour most worth testing after any
change to the routing path.

Omitted `routing` means the safe default: the local path.

---

## 7. Telemetry pipeline

Three streams, one correlation key (`request_id`), one socket writer.

```
control  → telemetry-control.jsonl   (decision_time_s)
frontend → telemetry-frontend.jsonl  (total_time_s)
gateway  → telemetry-gateway.jsonl   (elapsed_s, ctx/tokens)   ← relayed
                    │
                    ▼
        bee-feed-lattice.sh   (the ONLY sock writer)
                    │  {"source":"lattice","body":"..."}
                    ▼
              /run/bee/logs.sock  →  Bee terminal log screen
```

Design rules:

- **Binaries write files; the feeder writes the socket.** One writer, one
  contract. Adding an event source means adding a file to the feeder's tail
  list — never teaching a binary about the socket.
- **Branch on content, not filename.** The feeder identifies an event by which
  key it carries, because one `tail` covers all three files.
- **Errors are events too.** A gateway error carries an `error` field and is
  rendered distinctly, so failures appear on the display instead of vanishing.

**Crossing the host boundary.** The Mac and the Pi share no filesystem, so
gateway telemetry is *pulled*: the gateway buffers its last 256 events and
serves

```
GET /telemetry?since=<seq>
  →  { "seq": N, "oldest_seq": O, "boot": "<id>", "events": [ ... ] }
```

and the control plane's existing 10-second health loop appends new events to a
relay file on the Pi. Consequences worth knowing:

- The relay file lags reality by up to ~10 seconds.
- No extra long-running process is required on the Mac.

**The response carries continuity, not just position.** A cursor alone cannot
distinguish *"nothing new"* from *"there is a hole"* — and a hole that looks
like quiet is an event nobody ever sees. So the payload reports its own
identity and extent alongside the newest event:

| field | what it answers |
|---|---|
| `seq` | where the stream is now |
| `oldest_seq` | the oldest the bounded ring still holds — a cursor below it means events were evicted unread |
| `boot` | which gateway *process* issued these numbers; `seq` is in memory, so it restarts at 1 and "seq 1" from a new run is otherwise indistinguishable from the one already delivered |

A second failure mode hides in the same blindness: after a gateway restart the
new counter can legitimately read *lower* than the consumer's cursor, and if the
new run has not yet produced an event, `seq` and `oldest_seq` both look
harmless. The boot id is what makes that restart visible.

**The relay file is the durable record.** Both halves keep no cursor state of
their own across restarts; the consumer recovers its position *and* the boot id
from the last line of the file it wrote, and resumes there. Three behaviours
follow, and they are the whole of the restart contract:

- A **control-plane** restart resumes rather than seeding past the events that
  arrived while it was down. A genuinely fresh install (no relay file) still
  seeds without replaying, so a first start does not dump 256 stale events onto
  the screen.
- A **gateway** restart is detected by the changed boot id and resynced from
  `oldest_seq`, recovering what the ring still holds rather than skipping it.
- Events the ring had already **evicted** cannot be recovered. They are reported
  rather than silently omitted: the consumer writes a gap marker into the relay
  stream, which the feeder renders as an `ERROR` line on the display. A restart
  that cost nothing produces no marker — restarts are routine now, and a red
  line on every one would train the reader to ignore the real ones.

---

## 8. Failure semantics

| condition | response | intent |
|---|---|---|
| no healthy local gateway (local request) | `503` | refuse rather than leak to cloud |
| control plane unreachable | `503` | fail closed |
| available RAM below margin | `429` | protect the SSD from swap |
| provider error | `500` | surfaced, not swallowed |
| malformed body | `400` | — |

Every one of these also emits a telemetry event. **A silent failure is a bug**;
if a request can fail without a line on the display, the telemetry path is
incomplete.

---

## 9. Invariants (do not drift)

1. **One public entry point.** Clients talk to the frontend only.
2. **The Mac computes; the Pi routes.** No local model on the RPi4, ever.
3. **`LOCAL_ONLY` never falls back to cloud.** Failure is correct behaviour.
4. **The gateway translates; it does not decide.**
5. **No new infrastructure.** No message bus, no database, no orchestrator.
6. **The Bee project is not modified.** Lattice supplies feeders only.
7. **Protect the Mac's SSD.** Serialise local inference; enforce the memory
   margin; never let a request push the machine into sustained swap.
8. **No usernames, hostnames, or addresses in tracked files.** Use `<placeholders>`
   and environment variables.
9. **Telemetry shares one key.** `request_id` threads all three planes.

If a change requires breaking one of these, it is an architectural change —
write it down in [`docs/lattice-design.md`](../lattice-design.md) first, which
is the anti-drift anchor for this project.

---

## 10. Design decisions on record

**OpenAI API as the wire format.** Every client already speaks it. The cost is
one extension object (`routing`), which compatible SDKs pass through untouched
via `extra_body`.

**Capability aliases instead of model names.** Decouples client intent from
hardware. See §4.

**Serialised local inference.** A slot semaphore of 1 on the gateway. Throughput
is sacrificed — deliberately — because concurrent large models are what push the
Mac into swap, and swap is what wears the SSD.

**HTTP telemetry pull instead of an SSH relay.** An earlier design tailed the
gateway log on the Mac and piped it over SSH to a receiver on the Pi. It was
fragile (required a forced-command entry on the Mac, plus a second Mac process)
and was retired in favour of the pull described in §7.

**Continuity in the telemetry response, not a persisted counter.** The
alternative to reporting `oldest_seq` and a boot id was to persist the gateway's
counter across restarts, which would have made the numbers monotonic and the
problem disappear. It was rejected because it adds durable state to a process
that is *supposed* to be disposable — a file to lose or corrupt on the Mac — for
a benefit the response can carry for free. The consumer already had a durable
record (its own relay file) and a way to reach the gateway's history; it lacked
only the information to know when to use them.

**Streaming as opt-in SSE.** Real chat clients stream by default and would
otherwise render an empty reply. The gateway emits `chat.completion.chunk`
frames when the client sets `stream: true`, and the frontend forwards the flag
untouched. Both halves are required — see the gotcha below.

**Two supervisors, not one.** The Pi services are systemd *user* units; the Mac
gateway is a launchd LaunchAgent. The split is forced — macOS has no systemd —
but user-scope was chosen deliberately on the Pi too: the binaries already live
in the owning account's home, and that account has `Linger=yes`, so boot-time
start needs no root, no new service account, and no relocation. Until
2026-09-26 all three ran as plain processes and **nothing** restarted them after
a crash or a reboot.

**Configuration via `EnvironmentFile`, never inline.** Host-specific values
(namely the gateway's address) live outside the unit so no tailnet address is
ever committed. The file is deliberately *not* optional: the built-in default
points the gateway at localhost, which on the Pi is the Pi itself — a
misconfiguration that would otherwise surface only as unexplained 503s.

---

## 11. Gotchas

These have each cost real debugging time.

> **A struct decode drops fields as silently as a whitelist does.** The frontend
> rebuilt its proxy body from named fields and lost `tools`; the gateway then
> decoded into a struct with no `tools` field and lost them again, one hop
> later — and nothing in either handler mentioned the field, so neither looked
> wrong. A whitelist at least names what it discards. When proxying, forward the
> caller's body and rewrite only what routing requires.

- **Go sniffs `Content-Type`.** `json.NewEncoder(w).Encode(...)` without an
  explicit header yields `text/plain; charset=utf-8`. Set it.
- **A pre-stream failure can still be a status code; a post-stream failure
  cannot.** Once bytes are committed the status is final, so a streaming handler
  must decide whether the error happened before the first write.
- **Model options are only sent when explicitly set.** Ollama's defaults are
  not the gateway's defaults; `num_ctx` and friends must be included in the
  request to take effect.
- **`LATTICE_GATEWAY_TELEMETRY` is CWD-relative**, unlike every other path.
  Start the gateway from a known directory — the LaunchAgent pins
  `WorkingDirectory` for exactly this reason.
- **The health loop is on a 10-second timer.** A just-restarted gateway is
  reported unhealthy until the next probe. Tests that fire immediately after a
  restart will see spurious 503s.
- **A cursor is not a continuity check.** The telemetry cursor and the gateway's
  `seq` have different lifetimes — `seq` is in-memory on the Mac and restarts at
  1, the consumer's cursor persists on the Pi — and for a long time the protocol
  reported only `seq`, so a reset, an eviction, and a genuinely quiet stream were
  all indistinguishable. Each mismatch then dropped events silently: the cursor
  could sit ahead of a counter that had started over, and the seed-on-first-pull
  skipped whatever was already buffered. **Making the services supervised turned
  gateway restarts from rare into routine, which is what exposed it.** The fix is
  the continuity fields in §7; the durable lesson is that *any* resume protocol
  needs the consumer to be able to tell "nothing new" from "there is a hole."
- **A zero sentinel cannot also be a legitimate value.** The consumer needed to
  know whether its cursor meant anything yet, and encoded that as `cursor == 0`.
  But a resync can legitimately land the cursor on zero, at which point "unknown"
  and "known to be at zero" were the same number — so every later poll re-seeded
  and swallowed the next event. It took a live test (a request that never reached
  the relay file) to find. The fix was an explicit `known bool`; the sentinel had
  been doing two jobs and only one of them was visible.
- **Supervision is not systemd everywhere.** macOS has no systemd; the gateway is
  a launchd LaunchAgent in the GUI session. That session context is not
  incidental — it is how the gateway reaches Ollama, which runs as a GUI app.
- **journald is not persisted** for user units on the Pi. Debug the feeder by
  running it in the foreground; the servers write no logs you can query after
  the fact.
- **`pkill -f` can kill your own SSH session** when its command line contains
  the pattern — including a remote command you sent over SSH that merely
  *mentions* the process name. Kill by PID (`systemctl show -p MainPID`).
- **`pgrep -x` silently fails past 15 characters.** `pgrep -x lattice-frontend`
  matches nothing (the name is 16 chars) and prints a warning you may not read,
  so a "kill by name" loop quietly skips it. `lattice-gateway` (15) is fine;
  `lattice-frontend` is not. This is a procps limit, not a Lattice one.
- **`systemctl --user` addresses a user manager, not a host.** Run from an
  account that does not own the units, it reports `inactive` for services that
  are demonstrably running — a false negative about the *wrong* manager. Only
  `--machine=<owner>@.host` reaches the right one. Both this and the `pgrep` limit
  share a shape worth internalising: the tool answers confidently about something
  other than what you asked, so the *verification* is what lies, not the service.

---

## 12. Glossary

| term | meaning |
|---|---|
| **capability alias** | the client-facing `model` value (`local-brain`, `local-coder`) resolved per target |
| **control plane** | the routing brain on the RPi4 |
| **gateway** | the execution plane on the Mac; OpenAI→Ollama translation |
| **frontend** | the single public entry point |
| **`inference.v1`** | the OpenAI API plus the `routing` envelope |
| **`LOCAL_ONLY`** | privacy level that forbids cloud fallback |
| **latency class** | `interactive` (prefers cloud) or `batch` (prefers local) |
| **slot** | the single local-inference semaphore on the gateway |
| **feeder** | the one process that writes the Bee socket |
| **relay file** | the Pi-side copy of gateway telemetry pulled over HTTP |

---

## 13. Where to look next

| document | contents |
|---|---|
| [`docs/lattice-design.md`](../lattice-design.md) | the anti-drift anchor: frozen topology, verified resources, phase roadmap |
| [`docs/specs/`](../specs/) | per-component specifications |
| [`docs/README.md`](../README.md) | the full documentation index |
| `LLM INFERENCE ROUTING FOR HOMELAB.md` | the original design transcript (source of truth) |

---

Authored by [pegamonstro](https://github.com/pegamonstro) — The Bikini Club.
Licensed under the [Apache License 2.0](../../LICENSE).
