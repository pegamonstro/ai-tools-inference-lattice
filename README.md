# Lattice — Inference Lattice

A lightweight distributed inference **control and execution** system for a three-host homelab. It separates the *decision* of what inference should happen (control plane) from the *mechanism* that performs it (gateway), and schedules work across two asymmetric resources: a fast-but-scarce cloud subscription and a slow-but-free local LLM.

## Topology

| Host | Role | Inference |
|---|---|---|
| **RPi4** (`user@rpi4`, Debian 13, aarch64) | Control plane — policy, capability, health, fallback, concurrency gate | Cloud-only client |
| **Mac mac-gateway** (Apple M1, 16 GB) | Lattice Gateway — the ONLY substantive local-LLM host | `granite4:3b` (main), `gemma3:4b`, `command-r7b:7b`, `hermes3:8b` |
| **RPi3** (Alpine 3.24, OpenRC) | Security appliance + homelab services | Inference client only; never computes local models |

## Routing model

Routing is a function of `privacy` (LOCAL_ONLY / LOCAL_PREFERRED / CLOUD_ALLOWED), `latency_class` (interactive → cloud-first; batch → local-first), `parallelism`, and `cost`. Cloud = fast, max 3 models parallel, burns token budget. Local LLM = free + unlimited parallel, but slow (~10 tok/s warm, worse cold/long-context). See [docs/lattice-design.md](docs/lattice-design.md).

## Phases

0 charter/repo/baseline → 1 `inference.v1` protocol → 2 RPi4 Control → 3 Mac Gateway → 4 E2E integration → 5 failure/fallback → 6 RPi3 integration → 7 observability/hardening → 8 provider abstraction *(deferred)* → 9 advanced scheduling *(deferred)* → 10 Lattice 2.x.

## Documents

- [docs/lattice-design.md](docs/lattice-design.md) — design continuity doc (the anti-drift anchor).
- [docs/baseline.md](docs/baseline.md) — Phase 0 baseline of the three hosts + local-latency measurement.
- [docs/prompts/phase-0-bootstrap.md](docs/prompts/phase-0-bootstrap.md) — the Phase 0 autonomous prompt.
- `LLM INFERENCE ROUTING FOR HOMELAB/` — the original design conversation transcript (source of truth).
