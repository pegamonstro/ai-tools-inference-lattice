# Lattice Build Orchestration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn Hermes on the RPi4 into a build system — a one-shot cloud orchestrator decomposes work into unit briefs, and a serial dispatcher runs each unit on a pinned local model behind a deterministic gate.

**Architecture:** No new components. The orchestrator is a prompt template; the dispatcher is one bash script; state is files in a plan directory. Routing is free, because Lattice routes on the model name and a worker is pinned with `-m`. Local inference is single-slot, so units are dispatched strictly one at a time.

**Tech Stack:** bash (`build-run.sh`), Hermes CLI (`hermes chat --oneshot`), Lattice's existing OpenAI-compatible frontend, Go toolchain for the gates.

**Spec:** [docs/specs/lattice-build-orchestration.md](../specs/lattice-build-orchestration.md)

## Global Constraints

Copied verbatim from the spec; every task's requirements implicitly include them.

- Go standard library only. No new dependencies; no modules beyond the existing `go.mod`.
- No real usernames, hostnames, or IP addresses may appear in any tracked file.
- No speculative infrastructure (anti-drift rule 5): no DAG engine, scheduler daemon, job queue, state DB, or message bus.
- Local concurrency is 1. Units are dispatched strictly serially; a plan that assumes parallel local workers is wrong.
- Local models never verify. Gates are exit codes; semantic review is a cloud responsibility.
- Every unit must be verifiable by a command, never by an opinion.

**Prerequisites this plan does not cover** (spec §5, both unresolved): `/v1/embeddings` returns `404` through the frontend, which breaks Hermes's `auxiliary.embedding`; and neither telemetry layer emits token fields. Neither blocks the pilot.

---

### Task 1: Orchestrator prompt template

**Files:**
- Create: `deploy/orchestrator/orchestrator-prompt.md`
- Create (acceptance record): `deploy/orchestrator/acceptance/2026-09-26-lattice-stats.md`

**Interfaces:**
- Consumes: nothing.
- Produces: a template containing the literal marker `{{TASK_BRIEF}}`, whose output contract is parsed by Task 2 — specifically, line 1 of each unit must match `<!-- gate: KIND TARGET ; model: MODEL -->` with `KIND ∈ {build,test,fmt}` and `MODEL ∈ {local-brain,local-deep,cloud-coder}`.

The template is authored. Its content is the prompt validated on 2026-09-26, which produced the four-unit decomposition recorded in spec §6. Its three load-bearing properties are: the unit's first line is machine-readable; the worker prompt is capped at 120 words and must be self-contained; and a `Blocked requirements` section is mandatory, which is what catches a requirement the named files cannot satisfy.

- [ ] **Step 1: Write the acceptance task brief**

Create `deploy/orchestrator/acceptance/2026-09-26-lattice-stats.md` with the brief that was validated — the full text of `cmd/lattice-stats/main.go`, the two telemetry schemas with real sample lines, the verified facts that `target` is one of `mac-gateway`/`ollama-cloud-secondary`/`""` and that **no token field exists in either layer**, and the three section-2 requirements. (Spec §6 and §5.2 summarise all of it; the brief inlines it so the orchestrator needs no repository access.)

- [ ] **Step 2: Run the orchestrator against it**

```bash
ssh <rpi4> 'cat deploy/orchestrator/orchestrator-prompt.md deploy/orchestrator/acceptance/2026-09-26-lattice-stats.md' \
  | ssh <rpi4> 'hermes chat --query-file - --oneshot -Q -m kimi-k2.7-code:cloud --provider ollama-launch --max-turns 4 --run-budget 600'
```

- [ ] **Step 3: Verify the decomposition**

Expected: a numbered list of units, and a `Blocked requirements` section stating that "total token spend (Cloud)" cannot be delivered from `cmd/lattice-stats/main.go`. Record the raw output in the acceptance file.

- [ ] **Step 4: Verify the routing was real**

```bash
ssh <rpi4> 'tail -n 3 /var/log/lattice/telemetry-frontend.jsonl'
```

Expected: a new line whose `model` is `kimi-k2.7-code:cloud` and whose `target` is `ollama-cloud-secondary`. If it is absent, the orchestrator did not go through Lattice — stop and fix that before continuing.

- [ ] **Step 5: Commit**

```bash
git add deploy/orchestrator/
git commit -m "Add the build orchestrator prompt and its acceptance record"
```

---

### Task 2: The serial dispatcher

**Files:**
- Create: `deploy/orchestrator/build-run.sh`

**Interfaces:**
- Consumes: a plan directory of `unit-*.md` files, each with the line-1 directive from Task 1; a repository directory containing the Go module.
- Produces: `<plan-dir>/ledger.md`, `<plan-dir>/<unit>-report-<N>.md`, `<plan-dir>/<unit>-gate-<N>.log`; exit `0` when every unit's gate passes.

- [ ] **Step 1: Write the driver**

```bash
#!/usr/bin/env bash
# Serial unit dispatcher for the Lattice build orchestrator.
#
# Usage: build-run.sh <plan-dir> <repo-dir>
#
# Expects <plan-dir>/unit-*.md, each with a machine-readable directive on line 1:
#     <!-- gate: KIND TARGET ; model: MODEL -->
# Everything from line 3 onward is the brief handed to the worker on stdin.
#
# Local inference is single-slot, so units run strictly one at a time.
set -uo pipefail

PLAN_DIR=${1:?usage: build-run.sh <plan-dir> <repo-dir>}
REPO_DIR=${2:?usage: build-run.sh <plan-dir> <repo-dir>}

HERMES=${HERMES:-hermes}
PROVIDER=${PROVIDER:-ollama-launch}
RUN_BUDGET=${RUN_BUDGET:-900}
ESCALATION=${ESCALATION:-0}          # extra rungs to climb after a gate failure
LADDER=(local-brain local-deep cloud-coder)

LEDGER="$PLAN_DIR/ledger.md"
printf '# Build ledger — %s\n\n' "$PLAN_DIR" > "$LEDGER"

log() { printf '%s\n' "$*" >> "$LEDGER"; }
now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
die() { printf 'build-run: %s\n' "$*" >&2; exit 1; }

ladder_index() {
  local want=$1 i
  for i in "${!LADDER[@]}"; do
    if [ "${LADDER[$i]}" = "$want" ]; then printf '%s' "$i"; return 0; fi
  done
  return 1
}

# Whitelisted gates. No shell string is ever evaluated: the directive names a
# kind, and the kind maps to fixed argv, so a directive cannot smuggle a command.
run_gate() {
  local kind=$1 target=$2 out
  case "$kind" in
    build) ( cd "$REPO_DIR" && go build "$target" ) ;;
    test)  ( cd "$REPO_DIR" && go test "$target" ) ;;
    fmt)   out=$( cd "$REPO_DIR" && gofmt -l "$target" ) || return 1
           [ -z "$out" ] ;;
    *)     return 127 ;;
  esac
}

# Prints "KIND<TAB>TARGET<TAB>MODEL" from line 1, or nothing.
read_directive() {
  head -n 1 "$1" | sed -n \
    's|^<!-- *gate: *\([a-z]*\) *\([^;]*[^ ;]\) *; *model: *\([a-z-]*\) *-->$|\1\t\2\t\3|p'
}

shopt -s nullglob
units=( "$PLAN_DIR"/unit-*.md )
shopt -u nullglob
[ "${#units[@]}" -gt 0 ] || die "no unit-*.md in $PLAN_DIR"

for unit in "${units[@]}"; do
  name=$(basename "$unit" .md)

  directive=$(read_directive "$unit")
  [ -n "$directive" ] || die "$name: line 1 must be '<!-- gate: KIND TARGET ; model: MODEL -->'"

  kind=${directive%%$'\t'*}
  rest=${directive#*$'\t'}
  target=${rest%%$'\t'*}
  model=${rest##*$'\t'}

  rung=$(ladder_index "$model") || die "$name: model '$model' is not in the ladder (${LADDER[*]})"
  last=$(( rung + ESCALATION ))
  [ "$last" -ge "${#LADDER[@]}" ] && last=$(( ${#LADDER[@]} - 1 ))

  log "## $name"
  log ""
  log "- starting model: \`$model\`"
  log "- gate: \`$kind $target\`"

  attempt=0
  passed=0
  while [ "$rung" -le "$last" ]; do
    m=${LADDER[$rung]}
    log "- attempt $((attempt+1)) @ $(now) — \`$m\`"

    worker_status=0
    tail -n +3 "$unit" | "$HERMES" chat --query-file - --oneshot -Q \
      -m "$m" --provider "$PROVIDER" --max-turns 1 --run-budget "$RUN_BUDGET" \
      > "$PLAN_DIR/$name-report-$attempt.md" 2>&1 || worker_status=$?

    if [ "$worker_status" -ne 0 ]; then
      log "- worker exited $worker_status — escalating"
    elif run_gate "$kind" "$target" > "$PLAN_DIR/$name-gate-$attempt.log" 2>&1; then
      log "- gate PASSED on attempt $((attempt+1))"
      passed=1
      break
    else
      log "- gate FAILED on attempt $((attempt+1)) — see \`$name-gate-$attempt.log\`"
    fi

    attempt=$((attempt+1))
    rung=$((rung+1))
  done

  [ "$passed" -eq 1 ] || { log "- ESCALATION EXHAUSTED"; log ""; die "$name: gate never passed (ledger: $LEDGER)"; }
  log ""
done

printf 'build-run: all %d unit(s) passed — ledger at %s\n' "${#units[@]}" "$LEDGER"
```

- [ ] **Step 2: Make it executable and syntax-check it**

```bash
chmod +x deploy/orchestrator/build-run.sh
bash -n deploy/orchestrator/build-run.sh
```

Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
git add deploy/orchestrator/build-run.sh
git commit -m "Add the serial unit dispatcher with whitelisted gates"
```

---

### Task 3: Driver test suite

No inference is performed: a stub stands in for `hermes`, and a throwaway Go module makes the gates real.

**Files:**
- Create: `deploy/orchestrator/testdata/hermes-stub.sh`
- Create: `deploy/orchestrator/build-run_test.sh`

**Interfaces:**
- Consumes: `build-run.sh` from Task 2, honouring `HERMES` from the environment.
- Produces: nothing; a test suite that exits non-zero on failure.

- [ ] **Step 1: Write the stub**

```bash
#!/usr/bin/env bash
# Test stub standing in for `hermes`: consumes the brief on stdin, records which
# model it was asked to use, and succeeds. HERMES_STUB_FAIL=1 simulates a crash.
buf=$(cat)
model=""
while [ $# -gt 0 ]; do
  case "$1" in
    -m) model=$2; shift 2 ;;
    *)  shift ;;
  esac
done
first=$(printf '%s\n' "$buf" | head -n 1)
printf '%s|%s\n' "$model" "$first" >> "${ORDER:-/dev/null}"
[ "${HERMES_STUB_FAIL:-0}" = "1" ] && exit 1
printf 'stub report for %s\n' "$model"
exit 0
```

- [ ] **Step 2: Write the suite**

```bash
#!/usr/bin/env bash
# Tests for build-run.sh. No inference is performed.
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
DRIVER="$HERE/build-run.sh"
STUB="$HERE/testdata/hermes-stub.sh"
fail=0
case_no=0

check() { # description expected actual
  case_no=$((case_no+1))
  if [ "$2" = "$3" ]; then
    printf 'ok %d - %s\n' "$case_no" "$1"
  else
    printf 'NOT OK %d - %s\n  expected: %s\n  actual:   %s\n' "$case_no" "$1" "$2" "$3"
    fail=1
  fi
}

setup() {
  TMP=$(mktemp -d)
  mkdir -p "$TMP/plan" "$TMP/repo/cmd/x"
  ORDER="$TMP/order.txt"; export ORDER
  cat > "$TMP/repo/go.mod" <<'EOF'
module example.com/t

go 1.21
EOF
  cat > "$TMP/repo/cmd/x/main.go" <<'EOF'
package main

func main() {}
EOF
}
teardown() { rm -rf "$TMP"; }

unit() { # path gate_kind gate_target model brief
  { printf '<!-- gate: %s %s ; model: %s -->\n\n' "$2" "$3" "$4"
    printf '%s\n' "$5"; } > "$1"
}

# --- 1: a passing gate exits 0 and logs PASSED
setup
unit "$TMP/plan/unit-01.md" build ./cmd/x local-brain "Build it."
rc=0
HERMES="$STUB" "$DRIVER" "$TMP/plan" "$TMP/repo" >/dev/null 2>&1 || rc=$?
check "passing gate exits 0" "0" "$rc"
check "one dispatch only" "1" "$(grep -c '^- attempt' "$TMP/plan/ledger.md")"
check "ledger records the pass" "1" "$(grep -c 'gate PASSED on attempt 1' "$TMP/plan/ledger.md")"
teardown

# --- 2: a failing gate escalates through the ladder and gives up
setup
unit "$TMP/plan/unit-01.md" build ./cmd/nope local-brain "Build nothing."
rc=0
HERMES="$STUB" ESCALATION=2 "$DRIVER" "$TMP/plan" "$TMP/repo" >/dev/null 2>&1 || rc=$?
check "exhausted escalation exits non-zero" "1" "$rc"
check "three rungs attempted" "3" "$(grep -c '^- attempt' "$TMP/plan/ledger.md")"
check "climbed to cloud-coder" "1" "$(grep -c 'cloud-coder' "$TMP/plan/ledger.md")"
teardown

# --- 3: units run in lexical order
setup
unit "$TMP/plan/unit-01.md" build ./cmd/x local-brain "FIRST"
unit "$TMP/plan/unit-02.md" build ./cmd/x local-brain "SECOND"
HERMES="$STUB" "$DRIVER" "$TMP/plan" "$TMP/repo" >/dev/null 2>&1
check "dispatch order is lexical" "local-brain|FIRST
local-brain|SECOND" "$(cat "$TMP/order.txt")"
teardown

# --- 4: a malformed directive is refused before any dispatch
setup
printf 'no directive here\n\nBrief.\n' > "$TMP/plan/unit-01.md"
rc=0
HERMES="$STUB" "$DRIVER" "$TMP/plan" "$TMP/repo" >/dev/null 2>&1 || rc=$?
check "malformed directive exits non-zero" "1" "$rc"
check "nothing was dispatched" "0" "$(grep -c '^- attempt' "$TMP/plan/ledger.md")"
teardown

# --- 5: an unknown model is refused
setup
unit "$TMP/plan/unit-01.md" build ./cmd/x gpt-9 "Build it."
rc=0
HERMES="$STUB" "$DRIVER" "$TMP/plan" "$TMP/repo" >/dev/null 2>&1 || rc=$?
check "unknown model exits non-zero" "1" "$rc"
teardown

# --- 6: a crashed worker escalates rather than being gated
setup
unit "$TMP/plan/unit-01.md" build ./cmd/x local-brain "Crash."
rc=0
HERMES="$STUB" HERMES_STUB_FAIL=1 ESCALATION=1 "$DRIVER" "$TMP/plan" "$TMP/repo" >/dev/null 2>&1 || rc=$?
check "crashed worker exits non-zero" "1" "$rc"
check "crash escalates" "2" "$(grep -c '^- attempt' "$TMP/plan/ledger.md")"
check "crash is recorded as such" "2" "$(grep -c 'worker exited 1' "$TMP/plan/ledger.md")"
teardown

# --- 7: a fmt gate fails on unformatted code
setup
printf 'package main\nfunc  main(){}\n' > "$TMP/repo/cmd/x/main.go"
unit "$TMP/plan/unit-01.md" fmt ./cmd/x local-brain "Format it."
rc=0
HERMES="$STUB" "$DRIVER" "$TMP/plan" "$TMP/repo" >/dev/null 2>&1 || rc=$?
check "unformatted code fails the fmt gate" "1" "$rc"
teardown

[ "$fail" -eq 0 ] && printf '\nall %d checks passed\n' "$case_no"
exit "$fail"
```

- [ ] **Step 3: Run the suite**

```bash
chmod +x deploy/orchestrator/build-run_test.sh deploy/orchestrator/testdata/hermes-stub.sh
deploy/orchestrator/build-run_test.sh
```

Expected: fourteen `ok` lines and `all 14 checks passed`. The suite was run
against this script on 2026-09-26 and passes at 14/14.

- [ ] **Step 4: Commit**

```bash
git add deploy/orchestrator/build-run_test.sh deploy/orchestrator/testdata/
git commit -m "Test the dispatcher with a stub worker and real Go gates"
```

---

### Task 4: Install on the RPi4 and index the docs

**Files:**
- Create: `deploy/orchestrator/README.md`
- Modify: `docs/README.md` (the Specifications table)
- Modify: `docs/manual/operations-manual.md`

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces: an installed harness at `<hermes-home>/skills/lattice-orchestration/`, reachable as `hermes chat -s lattice-orchestration`.

- [ ] **Step 1: Write the README**

Document the three-command loop — assemble and run the orchestrator, review the emitted plan, run `build-run.sh` — plus the plan-directory layout, the `HERMES`/`PROVIDER`/`RUN_BUDGET`/`ESCALATION` environment variables, and the ladder. Note explicitly that the plan is reviewed by a cloud model **before** any worker runs, and why (spec §6).

- [ ] **Step 2: Index the spec**

Add to the Specifications table in `docs/README.md`:

```markdown
| [`lattice-build-orchestration.md`](specs/lattice-build-orchestration.md) | the build loop: macro orchestrator, pinned local workers, exit-code gates, the escalation ladder |
```

- [ ] **Step 3: Note it in the operations manual**

Add a short subsection: how to run a build plan, where the ledger and reports land, and that `ESCALATION` defaults to 0 so a unit stays on its declared model unless a rung is explicitly bought.

- [ ] **Step 4: Install on the Pi**

```bash
ssh <rpi4> 'mkdir -p ~/.hermes/skills/lattice-orchestration'
scp deploy/orchestrator/orchestrator-prompt.md <rpi4>:~/.hermes/skills/lattice-orchestration/SKILL.md
scp deploy/orchestrator/build-run.sh <rpi4>:~/bin/lattice-build-run.sh
```

Add `name: lattice-orchestration` and a `description:` line to the installed `SKILL.md` frontmatter, as Hermes requires.

- [ ] **Step 5: Commit**

```bash
git add docs/ deploy/orchestrator/README.md
git commit -m "Document and install the build orchestration harness"
```

---

### Task 5: Pilot acceptance — `lattice-stats` observability §2

This is the spec's exit test: run the whole loop on a real task and check that the decomposition survives contact.

**Files:**
- Create: `<plan-dir>/` on the RPi4 (not tracked)
- Modify: `cmd/lattice-stats/main.go`, `cmd/lattice-stats/main_test.go`

**Interfaces:**
- Consumes: Tasks 1–4.
- Produces: no new interface; the acceptance record.

- [ ] **Step 1: Produce the plan**

Run Task 1's orchestrator command. Expect `unit-01.md` … `unit-0N.md`. If the `Blocked requirements` section is missing, or no unit carries a line-1 directive, the prompt or the brief is wrong — fix that, don't hand-edit the units.

- [ ] **Step 2: Review the plan before dispatching**

Read the units against `docs/specs/lattice-observability.md` §2. Spec §6 records what a prior run got wrong and what to look for: whether the control layer's `decision_time_s` (routing, ~0.002 s) is being pooled with the frontend's `total_time_s` (end-to-end, ~2 s) into a single "average latency per target" — a figure true of neither layer. Also check for fields a unit produces that no later unit consumes. Correct the brief and re-run rather than editing the units.

- [ ] **Step 3: Dispatch**

```bash
ssh <rpi4> 'lattice-build-run.sh <plan-dir> <repo-dir>'
```

Expect a ledger with one entry per unit, and `all N unit(s) passed`.

- [ ] **Step 4: Verify the token requirement stayed blocked**

Confirm no unit invented a token field. `grep -rn 'prompt_tokens\|completion_tokens' cmd/lattice-stats/` must return nothing: the requirement is undeliverable until the telemetry layers emit those fields (spec §5.2).

- [ ] **Step 5: Run the gates from a clean tree**

```bash
go build ./... && go test ./... && gofmt -l cmd/lattice-stats/
```

Expected: build and test pass, `gofmt -l` prints nothing.

- [ ] **Step 6: Record the acceptance, then commit**

Write the ledger, the plan, and what the run got right and wrong into `deploy/orchestrator/acceptance/2026-09-26-lattice-stats.md`, replacing the Task 1 record with the end-to-end one.

```bash
git add cmd/lattice-stats/ deploy/orchestrator/acceptance/
git commit -m "Run the build loop end to end against the observability pilot"
```

---

## Self-review

**Spec coverage.** §1 corrected strategy → Tasks 1, 2, 5 (gates mechanical, review semantic). §2 economy → the serial dispatch in Task 2 and the 120-word worker cap in Task 1. §3.2 router-is-the-model-tag → the `-m` pin in Task 2, no routing code. §3.3 invocation → the `hermes chat` line in Task 2. §3.4 unit protocol → line-1 directive, Tasks 1 and 2. §3.5 gates → `run_gate`, Task 2. §3.6 review → Task 5 Step 2. §3.7 escalation → the ladder, Tasks 2 and 3. §4 non-goals → Global Constraints. §5 prerequisites → excluded and named. §6 evidence → Tasks 1 and 5. §8 recorded drift and §9 open questions → not implementation work; §9.3 is worth a follow-up spec.

**Type consistency.** `LADDER` is spelled identically in `build-run.sh` and its tests; the directive format in Task 1's output contract is the one `read_directive` parses in Task 2 and the one `unit()` writes in Task 3; `HERMES`, `PROVIDER`, `RUN_BUDGET`, `ESCALATION` are the same four names in Tasks 2, 3 and 4.

**Gaps.** Spec §5.1 (embeddings `404`) is a prerequisite for the orchestrator's memory, not for this loop, so it is excluded by design — but it must be resolved before the orchestrator is used for real work rather than a pilot.
