# LATTICE — PHASE 0 BOOTSTRAP

> Single-block autonomous prompt. Paste this whole block into Claude (or the DeepSeek Harness) and let it run to completion. Do not trim it. If you are asked to make a judgment call that this prompt does not cover, prefer the anti-drift default and record the decision in `docs/baseline.md`.

---

## Context

You are executing **Phase 0** of **Lattice** (Inference Lattice) — a lightweight distributed inference control/execution system for a three-host homelab. The design is frozen; do not redesign it. Your job is to establish the repository and record the unvarnished current state of the three hosts, and to resolve one open measurement question.

The frozen architecture:

- **RPi4** (`user@rpi4`, Debian 13 trixie, aarch64) = Control plane. Decides *what* inference should happen. No substantive local LLM.
- **Mac mac-gateway** (Apple M1, 16 GB) = Lattice Gateway. The ONLY substantive local-LLM host.
- **RPi3** (Alpine 3.24, OpenRC) = security appliance. Outside the control plane.

Anti-drift rules you must respect (10): (1) Ollama is an implementation detail; (2) the RPi4 decides, the Mac executes; (3) RPi3 stays a security appliance; (4) one canonical protocol; (5) no speculative infrastructure (no K8s/Redis/Kafka/Postgres-for-routing); (6) no premature provider abstraction; (7) deterministic infrastructure; (8) every phase has an exit test; (9) preserve reversibility; (10) complexity must earn its existence.

Routing model (already decided): routing is a function of `privacy` (LOCAL_ONLY / LOCAL_PREFERRED / CLOUD_ALLOWED), `latency_class` (interactive → cloud-first; batch → local-first), `parallelism`, and `cost`. Cloud = fast, **max 3 models parallel**, burns token budget. Local LLM = slow (possibly 30min+/turn) but free + unlimited parallel. See `docs/lattice-design.md` if present.

---

## Tasks

### 1. Establish the repository

Working directory is `/Users/archcore/Projects/LLM-router`. Do the following, and stop if the directory is already a git repo (report and continue):

1. `git init` if not already a repo.
2. Create `README.md` — a charter: project name (Lattice / Inference Lattice), one-paragraph purpose, the three-host topology, the phase list (0–10), and a pointer to `docs/lattice-design.md` as the continuity doc.
3. Ensure `docs/lattice-design.md` exists. If it does not, create it from the spec in this prompt (sections: Purpose, Topology, Resource tiers, Routing model, Privacy levels, Anti-drift rules, Phase roadmap, Resolved decisions, Open questions).
4. Create `docs/baseline.md` (you will fill it in step 3–4).
5. Commit with a clear message. Do NOT push (no remote exists).

### 2. Baseline the three hosts (record, do not change)

For each host, record in `docs/baseline.md`:

- **rpi4** — SSH `user@rpi4`. Record: OS + version (`cat /etc/os-release`), arch (`uname -m`), kernel (`uname -r`), toolchains (`go version`, `python3 --version`, presence of rustc/node), and the full inference surface: `curl -s http://127.0.0.1:11434/api/tags` (list every model with its `:cloud` or local suffix), and the Hermes provider config (`~/.hermes/config.yaml` `providers:` section only — do not print secrets).
- **Mac mac-gateway** — local machine. Record: `uname -m`, `sysctl -n machdep.cpu.brand_string`, `sysctl -n hw.memsize`, `ollama list` (every model + size).
- **rpi3** — SSH `user@rpi3` (or `user@rpi3` per your `~/.ssh/config`). Record: OS + version, arch, and confirm it is NOT running any local LLM inference (it must remain a security appliance).

Classify every model you find into one of the three resource tiers: **cloud** (`:cloud` / subscription), **Mac-local LLM**, **tiny/embedding**.

### 3. Resolve the local-latency question (the one measurement that matters)

The design depends on the *true* latency of Mac-local inference. Earlier docs claim `granite4:3b` ≈ 13.1 tok/s, but observed reality is sometimes 30min+/turn. Resolve this contradiction by measuring, on the Mac (`ollama` at `/usr/local/bin/ollama`, models `granite4:3b` primary, also `gemma3:4b`):

1. Warm the model, then time a short generation (a single ~100-token reply). Record tokens/sec and wall-clock.
2. Time the same on a **long-context** prompt (a few thousand tokens of context) — record prefill time separately if observable.
3. Time a **cold** request (after `ollama stop granite4:3b` or first call after idle) vs **warm**.
4. Repeat for `gemma3:4b` and `hermes3:8b` if feasible, to see if latency scales with model size.

Use a direct OpenAI-compatible call, e.g.:

```bash
time curl -s http://127.0.0.1:11434/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"granite4:3b","messages":[{"role":"user","content":"Say hello in one sentence."}],"stream":false}'
```

Record, in `docs/baseline.md`, a **local-latency table**: model, warm/cold, short/long context, wall-clock, tokens/sec. Then state the conclusion explicitly: **is Mac-local inference viable for interactive turns, or is it batch-only?** This single conclusion gates the Phase 2 routing policy, so be precise and do not hand-wave.

### 4. Write the baseline conclusion

End `docs/baseline.md` with a "Phase 0 exit test" section:

- Repo exists and is committed: yes/no.
- Three hosts baselined: yes/no, with the model-tier classification.
- Local-latency question resolved: yes/no, with the one-sentence conclusion.
- Any deviations from the frozen design or this prompt, recorded with reason.

Commit `docs/baseline.md`.

---

## Exit test

Phase 0 is **done** when all of the following are true:

1. `git init` done; `README.md` charter and `docs/lattice-design.md` exist and are committed.
2. `docs/baseline.md` records the three hosts' OS/arch/toolchains/inference-surface with models classified into the three tiers.
3. The local-latency measurement is done and its conclusion (interactive-viable vs batch-only) is stated in one sentence.
4. Nothing was changed on any host (read-only baselining).

Report back with: a summary of the baseline, the model-tier classification, and the local-latency conclusion. Do not start Phase 1.
