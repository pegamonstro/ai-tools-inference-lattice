# Spec scope: LAT-AFM-0 (task `ai-tools-inferen-chief-of-staff-c832ae`)

**Kind:** spec (docs only)
**Project:** `ai-tools-inference-lattice` (`pegamonstro/ai-tools-inference-lattice`)
**Branch:** `cursor/apple-foundation-models-spec-bb00`
**Goal:** Land the docs-only AFM provider spec and correct one stale registry claim.

## Context read (researcher host)

| path | result |
|---|---|
| `.cursor/rules/duo.mdc` | **missing** in this clone — not present under `/workspace` |
| `agents/PLAN.md`, `agents/STATE.json`, `agents/DECISIONS.md` | **missing** in this clone |
| Open issues / PRs (`pegamonstro/ai-tools-inference-lattice`) | **none open** at research time (GitHub list empty) |
| CoS source `~/work/seed-fm-research-20261119/AFM-LEGAL-AND-LATTICE.md` Part C (sha256 prefix `077b714f`) | **not on this VM** — normative licence/URL text taken from the CoS task brief; João should confirm Part C parity |

## Deliverables in this change

| path | role |
|---|---|
| [`docs/specs/lattice-afm-provider.md`](lattice-afm-provider.md) | Product spec: sections 1–7, licence conditions verbatim, supported Lattice URL, kill switch default OFF |
| [`docs/specs/lattice-apple-acceleration.md`](lattice-apple-acceleration.md) | Single-line correction: registry is **not** hardcoded to ollama |
| this file | Agent-duo scoping record for DSH validation |

## Claims DSH must verify (with citations)

1. **Stale line fixed.** `lattice-apple-acceleration.md` P2 no longer says the providers registry is hardcoded to `"ollama"`; it points at `LATTICE_GATEWAY_PROVIDERS` / `cmd/lattice-gateway/providers.go`.
2. **Registry kinds today.** `loadProviders` accepts `ollama`, `mlx`, `mflux`/`sdxl`, `speech` and rejects unknown kinds — `cmd/lattice-gateway/providers.go:103-114`.
3. **AFM kind not implemented.** Grep shows no `"afm"` case in `cmd/lattice-gateway/providers.go` (spec is future-facing).
4. **Frontend bind mechanism.** `LATTICE_FRONTEND_BIND` parses comma-separated `ip:port` — `cmd/lattice-frontend/main.go:437-457`. Concrete IPs `100.80.26.89` / `192.168.0.10` and nftables are CoS/operator facts, not committed in tracked deploy env files.
5. **Licence block verbatim.** `lattice-afm-provider.md` Licence section lists the eight brief conditions without paraphrase (personal non-commercial; local-only listener / never networked `fm serve` or helper; concurrency 1; no train/fine-tune/distil/embed; no portal or X-Wars; human review before CANONICAL; provenance tag; S.E.E.D. only via `http://100.80.26.89:8080/v1`).
6. **URL table.** Only supported URL `http://100.80.26.89:8080/v1`; `http://192.168.0.10:8080` marked UNSUPPORTED.
7. **Diff scope.** Only `docs/**` markdown touched — no `*.go`, `deploy/**`, config, env, plist, script, or binary.

## Open questions for João (do not decide in DSH)

Copied from [`lattice-afm-provider.md`](lattice-afm-provider.md); DSH must not close them:

1. Helper implementation choice (custom local helper vs other), provided never networked `fm serve`.
2. AFM mapped-but-down: fail-loudly vs local-only re-resolve.
3. Provenance tag string / telemetry field.
4. Capability alias vs literal model ids.
5. Confirm L1/L2/L3 naming matches Part C of `AFM-LEGAL-AND-LATTICE.md`.

## Exact DSH check (“done” for this stage)

Run from the PR head (docs-only):

```bash
test -f README.md
test -f docs/specs/lattice-afm-provider.md
test -f docs/specs/ai-tools-inferen-chief-of-staff-c832ae.md
# stale claim gone
! grep -n 'hardcoded to `"ollama"`\|hardcoded to "ollama"' docs/specs/lattice-apple-acceleration.md
# licence / URL anchors present
grep -F 'personal non-commercial use only' docs/specs/lattice-afm-provider.md
grep -F 'http://100.80.26.89:8080/v1' docs/specs/lattice-afm-provider.md
grep -F 'http://192.168.0.10:8080' docs/specs/lattice-afm-provider.md
grep -F 'UNSUPPORTED' docs/specs/lattice-afm-provider.md
# sections 1–7 headings exist
grep -E '^## [1-7]\.' docs/specs/lattice-afm-provider.md | wc -l   # expect 7
# diff touches only docs markdown
git diff --name-only origin/main...HEAD | grep -v '^docs/.*\.md$' && echo FAIL || echo OK
# secret scan (no private keys / tokens in the new docs)
! grep -E 'BEGIN (RSA |OPENSSH )?PRIVATE KEY|AKIA[0-9A-Z]{16}|xox[baprs]-' docs/specs/lattice-afm-provider.md docs/specs/ai-tools-inferen-chief-of-staff-c832ae.md docs/specs/lattice-apple-acceleration.md
```

**Done means:** all checks above pass; PR is docs-only; AFM remains disabled (no code/config/deploy). Next **plan/build** stage may implement `kind: "afm"` only after João answers the open questions and §7 acceptance-before-enable is green — that stage is **not** started by this PR.
