# Lattice — Inference Lattice

```
██╗      █████╗ ████████╗████████╗██╗ ██████╗███████╗
██║     ██╔══██╗╚══██╔══╝╚══██╔══╝██║██╔════╝██╔════╝
██║     ███████║   ██║      ██║   ██║██║     █████╗
██║     ██╔══██║   ██║      ██║   ██║██║     ██╔══╝
███████╗██║  ██║   ██║      ██║   ██║╚██████╗███████╗
╚══════╝╚═╝  ╚═╝   ╚═╝      ╚═╝   ╚═╝ ╚═════╝╚══════╝

          I N F E R E N C E   L A T T I C E
          > sovereignty over speed
```

A lightweight distributed inference **control and execution** system for a three-host homelab. It separates the *decision* of what inference should happen (control plane) from the *mechanism* that performs it (gateway), and schedules work across two asymmetric resources: a fast-but-scarce cloud subscription and a slow-but-free local LLM.

## Topology

| Host | Role | Inference |
|---|---|---|
| **RPi4** (Debian 13, aarch64) | Control plane — policy, capability, health, fallback, concurrency gate | Cloud-only client |
| **Mac** (Apple M1, 16 GB) | Lattice Gateway — the ONLY substantive local-LLM host | `granite4:3b` (main), `gemma3:4b`, `command-r7b:7b`, `hermes3:8b` |
| **RPi3** (Alpine 3.24, OpenRC) | Security appliance + homelab services | Inference client only; never computes local models |

## Routing model

Routing is a function of `privacy` (LOCAL_ONLY / LOCAL_PREFERRED / CLOUD_ALLOWED), `latency_class` (interactive → cloud-first; batch → local-first), `parallelism`, and `cost`. Cloud = fast, max 3 models parallel, burns token budget. Local LLM = free + unlimited parallel, but slow (~10 tok/s warm, worse cold/long-context). See [docs/lattice-design.md](docs/lattice-design.md).

## Phases

0 charter/repo/baseline → 1 `inference.v1` protocol → 2 RPi4 Control → 3 Mac Gateway → 4 E2E integration → 5 failure/fallback → 6 RPi3 integration *(out of scope)* → 7 observability/hardening → 8 provider abstraction → 9 advanced scheduling → 10 Lattice 2.x.

## Using it

Lattice exposes a single OpenAI-compatible endpoint. Point any OpenAI client at
the frontend and set `model` to a **capability alias** (`local-brain`,
`local-coder`) rather than a model name — the control plane resolves it per
target. Streaming (`"stream": true`) is supported.

```bash
curl -X POST http://<frontend-host>:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"local-brain","messages":[{"role":"user","content":"Hello."}]}'
```

Full client instructions — routing envelope, streaming, worked examples, error
shapes — are in the [User Guide](docs/guide/user-guide.md).

## Documents

| document | for |
|---|---|
| [docs/README.md](docs/README.md) | **documentation index** — start here |
| [docs/guide/user-guide.md](docs/guide/user-guide.md) | *using* Lattice from a client |
| [docs/manual/operations-manual.md](docs/manual/operations-manual.md) | *running* Lattice: build, deploy, env vars, troubleshooting |
| [docs/handbook/architecture-handbook.md](docs/handbook/architecture-handbook.md) | *changing* Lattice: invariants, lifecycle, telemetry, gotchas |
| [docs/lattice-design.md](docs/lattice-design.md) | design continuity doc (the anti-drift anchor) |
| [docs/specs/](docs/specs/) | per-component specifications |
| [docs/baseline.md](docs/baseline.md) | Phase 0 baseline of the hosts + local-latency measurement |
| [docs/prompts/phase-0-bootstrap.md](docs/prompts/phase-0-bootstrap.md) | the Phase 0 autonomous prompt |
| [`LLM INFERENCE ROUTING FOR HOMELAB/`](LLM%20INFERENCE%20ROUTING%20FOR%20HOMELAB/) | the original design conversation transcript (source of truth) |

---

Authored by [pegamonstro](https://github.com/pegamonstro) — The Bikini Club. Licensed under the [Apache License 2.0](LICENSE).
