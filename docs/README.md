# Lattice Documentation

An index of everything written about Lattice, grouped by what you are trying to
do. Start at the row that matches your situation.

---

## By audience

| you are… | start here |
|---|---|
| **using** Lattice from a client | [User Guide](guide/user-guide.md) |
| **running / deploying** it | [Operations Manual](manual/operations-manual.md) |
| **changing** it | [Architecture Handbook](handbook/architecture-handbook.md) |
| **new to the project** | [README](../README.md) → [README_PUBLIC](../README_PUBLIC.md) |
| **looking for a component's contract** | [Specifications](specs/) |

---

## The three main documents

### [User Guide](guide/user-guide.md)
Client-facing. The OpenAI-compatible entry point, the capability-alias model
field, the `routing` envelope, streaming, worked `curl`/Python examples, and
the error shapes you can expect.

### [Operations Manual](manual/operations-manual.md)
Operator-facing. Cross-compilation, the full environment-variable reference,
running by hand, process supervision, deploying a new binary and rolling back,
the telemetry/Bee pipeline, Mac memory safety, and a troubleshooting table.

### [Architecture Handbook](handbook/architecture-handbook.md)
Contributor-facing. The frozen topology, the capability model, the request
lifecycle, the routing policy table, the telemetry pipeline, the list of
**invariants that must not drift**, decisions on record, and the gotchas that
have each cost real debugging time.

---

## Reference material

| document | contents |
|---|---|
| [`lattice-design.md`](lattice-design.md) | **anti-drift anchor.** Frozen topology, verified resource tiers, routing model, phase roadmap. Update this *first* when architecture changes. |
| [`baseline.md`](baseline.md) | the measured baseline the design was built against |
| [`prompts/phase-0-bootstrap.md`](prompts/phase-0-bootstrap.md) | the autonomous bootstrap prompt for Phase 0 |
| [`specs/`](specs/) | per-component specifications (see below) |
| [`../PROJECT_DOCS.md`](../PROJECT_DOCS.md) | control/data-plane split, hardware mapping, deployment order, maintenance |
| [`../README_PUBLIC.md`](../README_PUBLIC.md) | public-facing overview: the problem, the architecture, the protocol |
| [`../NOTICE`](../NOTICE) · [`../LICENSE`](../LICENSE) | attribution and licence (Apache 2.0) |
| `LLM INFERENCE ROUTING FOR HOMELAB.md` | the original design transcript — the source of truth this project was built from |

---

## Specifications

| spec | covers |
|---|---|
| [`inference-v1.md`](specs/inference-v1.md) | the `inference.v1` protocol — OpenAI API + `routing` envelope |
| [`lattice-control.md`](specs/lattice-control.md) | the control plane: capability map, health, routing decision |
| [`lattice-frontend.md`](specs/lattice-frontend.md) | the public entry point and proxy behaviour |
| [`lattice-gateway.md`](specs/lattice-gateway.md) | the Mac execution plane: Ollama translation, memory guard |
| [`lattice-provider.md`](specs/lattice-provider.md) | local vs cloud provider abstraction |
| [`lattice-scheduling.md`](specs/lattice-scheduling.md) | latency classes, concurrency, slot semantics |
| [`lattice-observability.md`](specs/lattice-observability.md) | telemetry streams and the Bee feeder contract |
| [`lattice-failure.md`](specs/lattice-failure.md) | failure semantics and the `LOCAL_ONLY` guarantee |
| [`lattice-agent-surface.md`](specs/lattice-agent-surface.md) | the agent-facing surface: body passthrough, literal model names, discovery endpoints, tool calling, the context ceiling |
| [`lattice-rpi3.md`](specs/lattice-rpi3.md) | the retired RPi3 security-appliance spec — **out of scope**, kept as a record |

---

## Conventions

- **Placeholders.** Addresses, hostnames, and model names that vary by
  installation appear as `<placeholder>`. No real usernames, hostnames, or IP
  addresses appear in tracked files.
- **Environment variables** are prefixed `LATTICE_` and documented in the
  [Operations Manual §3](manual/operations-manual.md#3-environment-variables).
- **Correlation.** Every request carries a `request_id` that threads the
  control, frontend, and gateway telemetry streams.

---

Authored by [pegamonstro](https://github.com/pegamonstro) — The Bikini Club.
Licensed under the [Apache License 2.0](../LICENSE).
