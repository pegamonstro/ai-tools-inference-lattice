# Lattice Operations Manual

How to build, deploy, run, and repair Lattice. This is the operator-facing
document — it assumes shell access to both hosts.

For the *why* behind these steps, read the
[Architecture Handbook](../handbook/architecture-handbook.md). For client
usage, read the [User Guide](../guide/user-guide.md).

---

## 1. Topology

Two hosts. There is no third.

| role | host | runs | listens |
|---|---|---|---|
| **control plane** | RPi4 (Debian 13) | `lattice-control`, `lattice-frontend`, feeder | `:8082` control, `:8080` frontend |
| **gateway** | Mac (Apple silicon) | `lattice-gateway`, Ollama (local models) | `:8081` gateway, `:11434` Ollama |
| **cloud provider** | external | Ollama cloud models | `:11434` on the Pi host |

The two hosts reach each other over a private overlay network (a tailnet).
Addresses are supplied by environment; nothing is hard-coded.

**Hard hardware rules.** The Mac is the *only* host permitted to run local
models. The RPi4 is cloud-only — it must never attempt local inference, because
its hardware cannot sustain it. Requests from the Pi that ask for local
inference are forwarded to the Mac; they are never computed on the Pi.

---

## 2. Build

The module is `github.com/pegamonstro/ai-tools-inference-lattice`, Go 1.27+.
Binaries are cross-compiled — build *for* the target on whatever machine you
like.

```bash
# Mac gateway (Apple silicon)
GOOS=darwin GOARCH=arm64 go build -o bin/lattice-gateway ./cmd/lattice-gateway

# RPi4 (Debian 13, arm64)
GOOS=linux GOARCH=arm64 go build -o bin/lattice-control  ./cmd/lattice-control
GOOS=linux GOARCH=arm64 go build -o bin/lattice-frontend ./cmd/lattice-frontend
GOOS=linux GOARCH=arm64 go build -o bin/lattice-cli      ./cmd/lattice-cli
```

`bin/` is gitignored. Components:

| binary | purpose |
|---|---|
| `lattice-control` | routing brain: holds capability map, health state, decides target |
| `lattice-frontend` | the single OpenAI-compatible entry point; proxies to the decision |
| `lattice-gateway` | runs on the Mac; translates OpenAI → Ollama, enforces memory limits |
| `lattice-cli` | thin prompt wrapper for manual checks |
| `lattice-stats`, `ref-client`, `ref-server` | helpers / reference implementations |

---

## 3. Environment variables

Every address and tunable is environment-driven so no hostname appears in the
source. Defaults are shown; override in each process's environment (see §5).

### Control plane (RPi4)

| variable | default | meaning |
|---|---|---|
| `LATTICE_CONTROL_ADDR` | `:8082` | listen address for `/route` and `/status` |
| `LATTICE_GATEWAY_URL` | `http://localhost:8081` | where the gateway lives (set to the Mac's tailnet address) |
| `LATTICE_OLLAMA_URL` | `http://localhost:11434` | the cloud Ollama endpoint on this host |
| `LATTICE_GATEWAY_TELEMETRY_LOCAL` | `/var/log/lattice/telemetry-gateway.jsonl` | relay file for gateway telemetry pulled over HTTP |

### Frontend (RPi4)

| variable | default | meaning |
|---|---|---|
| `LATTICE_FRONTEND_ADDR` | `:8080` | the public entry point |
| `LATTICE_CONTROL_URL` | `http://127.0.0.1:8082/route` | control plane decision endpoint |
| `LATTICE_FRONTEND_TELEMETRY` | `/var/log/lattice/telemetry-frontend.jsonl` | frontend telemetry sink |

### Gateway (Mac)

| variable | default | meaning |
|---|---|---|
| `LATTICE_GATEWAY_ADDR` | `:8081` | gateway listen address |
| `LATTICE_OLLAMA_URL` | `http://localhost:11434` | local Ollama |
| `LATTICE_GATEWAY_MAX_CONTEXT` | `32768` | context window ceiling |
| `LATTICE_GATEWAY_KV_CACHE` | `q8_0` | KV cache quantisation |
| `LATTICE_GATEWAY_MEMORY_MARGIN_MB` | `1536` | free RAM the gateway refuses to cross (see §7) |
| `LATTICE_GATEWAY_TELEMETRY` | `telemetry-gateway.jsonl` | gateway's local event buffer sink (CWD-relative) |

> **Note.** The control plane's telemetry sink is the one hard-coded path
> (`/var/log/lattice/telemetry-control.jsonl`); it is not environment-driven.
> Everything else above is.

---

## 4. Run it by hand

Useful for debugging, and for seeing exactly what each process needs to run.

```bash
# On the Mac
LATTICE_GATEWAY_ADDR=:8081 ./bin/lattice-gateway

# On the RPi4
LATTICE_GATEWAY_URL=http://<mac-tailnet-address>:8081 ./bin/lattice-control
LATTICE_CONTROL_URL=http://127.0.0.1:8082/route ./bin/lattice-frontend
```

Health checks:

```bash
curl -s http://<mac-tailnet-address>:8081/health      # gateway
curl -s http://127.0.0.1:8082/status                  # control; shows gateway health
curl -s http://127.0.0.1:8080/v1/chat/completions ... # frontend (see User Guide)
```

`/status` reports per-gateway health as JSON. The control plane re-probes the
gateway every 10 seconds, so after restarting the gateway allow up to ~10s
before `local` requests route again.

---

## 5. Process supervision

All four services are supervised. Nothing has to be started by hand, and
everything comes back after a crash or a reboot.

| service | host | supervisor | unit |
|---|---|---|---|
| `lattice-control` | Pi | systemd **user** unit (runs as the lattice account) | `lattice-control.service` |
| `lattice-frontend` | Pi | systemd **user** unit | `lattice-frontend.service` |
| `lattice-gateway` | Mac | launchd **LaunchAgent** | `com.lattice.gateway` |
| Bee feeder | Pi | systemd user unit | `bee-feed-lattice.service` |

macOS has no systemd. The gateway's supervisor is a per-user **LaunchAgent**
loaded into the GUI session — which is also why it can reach the Ollama.app
instance, since that runs as a GUI app rather than a system service.

Both Pi units are `WantedBy=default.target` and enabled, and the account has
`Linger=yes`, so they start at boot without anyone logging in. All three run
with an automatic-restart policy (`Restart=always` / `KeepAlive`).

### Managing the Pi services

```bash
systemctl --user status  lattice-control.service
systemctl --user restart lattice-frontend.service
```

From another account on the same host, `--machine` targets the owner's user
manager:

```bash
sudo systemctl --user --machine=<lattice-account>@.host restart lattice-control.service
```

### Managing the Mac gateway

```bash
launchctl print      gui/$UID/com.lattice.gateway
launchctl kickstart -k gui/$UID/com.lattice.gateway
```

Logs are at `~/Library/Logs/lattice/lattice-gateway.{out,err}.log`.

### Installing

Both are installed from the repo:

```bash
./deploy/install-macos.sh          # on the Mac
```

The Pi units are copied into `~/.config/systemd/user/` with a matching
`~/.config/lattice/*.env`, then `systemctl --user daemon-reload` and
`enable --now`. They use `%h`, so no path needs editing — the install script
exists only on the Mac, where launchd cannot expand variables and the plist
needs absolute paths substituted.

### Deploying a new binary

Overwriting a *running* binary fails with `Text file busy`. Stop the service,
copy, start:

```bash
systemctl --user stop lattice-frontend
cp bin/lattice-frontend ~/bin/lattice-frontend.new
mv ~/bin/lattice-frontend.new ~/bin/lattice-frontend
systemctl --user start lattice-frontend
```

On the Mac, `launchctl bootout gui/$UID/com.lattice.gateway` first (or just
re-run `install-macos.sh`, which boots out before re-bootstrapping).

> **Kill by PID, not by pattern.** When you must kill a process directly,
> `pkill -f "<name>"` matches the *full command line* — including your own SSH
> session when its command text contains that name, so it can kill your session
> mid-deploy. (It has happened.) Use the service manager instead; if you must
> kill, take the PID from `systemctl show -p MainPID` and `kill` it.

Rollback is the same procedure with the previous binary — keep a copy before
deploying.

---

## 6. Telemetry and the Bee terminal display

Lattice surfaces every routing decision, completion, and execution as a line on
the Bee terminal's log screen. **The Bee project itself is never modified** —
Lattice only writes to its existing socket contract.

The invariant is:

> binaries write telemetry **JSONL** → one feeder tails the files and is the
> **only** thing that writes the socket.

Three streams, all keyed by `request_id`, all under `/var/log/lattice/`:

| file | written by | distinguishing key |
|---|---|---|
| `telemetry-control.jsonl` | control plane | `decision_time_s` |
| `telemetry-frontend.jsonl` | frontend | `total_time_s` |
| `telemetry-gateway.jsonl` | **control plane** (relayed) | `elapsed_s` |

The feeder ([`deploy/bee-feed-lattice.sh`](../../deploy/bee-feed-lattice.sh))
tails all three with `tail -n0 -F` and branches on which key is present — not on
which file the line came from. Error events carry an `error` field and render as
`<id> <model> ERROR <message>`.

### Why the gateway file is written by the control plane

The Mac shares no filesystem with the Pi. The gateway therefore keeps the last
256 events in memory and serves them over HTTP:

```
GET http://<mac>:8081/telemetry?since=<seq>  →  { "seq": N, "events": [ ... ] }
```

The control plane pulls this in its existing 10-second health loop and appends
only new events to the relay file. The first pull seeds the cursor without
replaying history. This replaced an earlier SSH-pipe relay and requires **no
long-running process on the Mac** beyond the gateway itself.

> **Known gap: events are lost across restarts.** The gateway's `seq` counter
> lives in memory and resets to zero when the gateway restarts, while the control
> plane's cursor persists on the Pi. After a gateway restart the control plane
> can therefore sit ahead of a counter that started over, silently relaying
> nothing until the gateway's `seq` climbs past the old value. Separately, the
> control plane's seed-on-first-pull skips anything already buffered at that
> moment. Both are telemetry-only — inference is unaffected — but a dropped
> event never appears on the Bee screen. Restarting the **control plane** as well
> as the gateway clears the stranded cursor in the meantime; a proper fix is
> tracked in the project's task list.

### Installing the feeder

`deploy/bee-feed-lattice.service` is a user unit; `%h` expands to the feeder
account's home. Install the script into `%h/work/bee-feeders/bin/`, where
`bee-feed-lib.sh` (part of the Bee terminal's own tooling) lives. Override
`BEE_FEED_LIB` to point elsewhere.

> **To add an event source:** write another JSONL stream and add it to the
> feeder's tail list. Never teach a binary to write the Bee socket directly.

---

## 7. Memory safety on the Mac

The Mac's SSD is a wear item, and swap thrashing is the fastest way to destroy
it. The gateway is built around that fact:

- **Memory margin.** The gateway refuses a request when available RAM is below
  `LATTICE_GATEWAY_MEMORY_MARGIN_MB` (default 1536 MiB), returning `429`
  instead of pushing the machine into swap.
- **Serialised inference.** Local inference runs one request at a time
  (a slot semaphore), so two large models never coexist in RAM.
- **KV cache quantisation.** `q8_0` by default keeps the cache small.
- **Cloud concurrency cap.** Cloud requests are gated at 3 in parallel.

**Operational note.** The Mac's hibernation mode writes a full-size sleepimage
to disk on every sleep — this, not swap, is the largest recurring disk write.
If SSD wear is a concern, that trade-off is the one worth revisiting.

To confirm the machine is *not* thrashing (the thing that actually wears the
disk), watch swapout traffic, not `vm.swapusage`:

```bash
sysctl vm.swapusage
vm_stat | grep -i swap          # Swapouts is the wear-relevant counter
```

---

## 8. Troubleshooting

| symptom | cause | fix |
|---|---|---|
| `503 No healthy local gateway found` | gateway down, or the Pi's 10s health loop hasn't re-probed it yet | the unit restarts it; wait ~10s; check `/status` |
| `429 Memory pressure` | free RAM below the margin | close memory hogs on the Mac, or lower the context window |
| `500 ollama returned status 404` | model id not pulled / not present in Ollama | `ollama pull <model>`, verify the capability map |
| `Text file busy` on deploy | the binary is running | stop the service, copy, start — see §5 |
| Frontend returns empty reply to a streaming client | a rebuild dropped the `stream` field somewhere on the proxy path | verify the field survives frontend → gateway |
| Cloud request returns empty model | client sent a real model name instead of a capability alias | use `local-brain` / `local-coder` |
| Nothing on the Bee screen | feeder not running, or relay file not yet created | run the feeder in the foreground with `2>/tmp/feeder.log`; remember journald isn't persisted here |
| Telemetry stops after a restart | gateway `seq` reset while the control plane's cursor stayed ahead | restart `lattice-control` too — see §6 |
| Unit won't start after an env edit | the `EnvironmentFile` is missing or unreadable (it is **not** optional) | check `~/.config/lattice/*.env` exists and is owned by the service account |

**Killing processes.** Prefer the service manager. When you must kill directly,
use the PID — `pkill -f "<name>"` can match the SSH session whose own command
line contains the name, killing your own session. `systemctl show -p MainPID`
gives the right PID without pattern-matching.

**Piping into Python.** A long-running `python3` in a pipeline needs `-u`, or
its output buffers and the pipeline stalls.

---

## 9. Maintenance checklist

- [ ] all three services active (`systemctl --user is-active lattice-control
      lattice-frontend` on the Pi; `launchctl print gui/$UID/com.lattice.gateway`
      on the Mac)
- [ ] `/status` reports the gateway healthy
- [ ] feeder unit active; all three telemetry files exist and are growing
- [ ] `bin/` on both hosts matches the current commit
- [ ] a warm local request completes end-to-end (User Guide §5)
- [ ] a `LOCAL_ONLY` request fails cleanly (503) when the gateway is stopped —
      verify it does **not** reach the cloud
- [ ] swapout counter is flat under idle and under one inference

---

Authored by [pegamonstro](https://github.com/pegamonstro) — The Bikini Club.
Licensed under the [Apache License 2.0](../../LICENSE).
