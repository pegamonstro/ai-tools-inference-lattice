# Spec: Lattice Model Repo

**Status:** Implemented (Phase: model offsite 1)
**Host:** RPi4 (store volume), any host (sync client)

The Macs run out of local disk because model caches grow without bound
(~120 GB of Ollama + HF/mflux blobs on the biggest gate host). The model repo
moves the *dormant copy* off-host: a content-addressed blob store on the Pi's
large partition. A Mac pushes a model's blobs to the repo, verifies them
server-side, deletes the local copy, and can restore byte-identical files
later. The repo stores bytes; it computes nothing (invariant 2) and it never
touches a Mac's model directories remotely (invariant 4 discipline) — eviction
is always a human `rm` after a verified push.

## 1. Shape

- `cmd/lattice-modelrepo` — stdlib Go service, plain `net/http`, no DB.
- Store root: `LATTICE_MODELREPO_STORE` (default `/mnt/offsite`), fixed
  subtree `inference-repo/blob-llm/` so the volume can hold other data.
- Telemetry: JSONL, `LATTICE_MODELREPO_TELEMETRY`, one row per request with
  `request_id` (honors `X-Request-Id`), `error` on every exit path
  (invariants 9, 4).
- Config via `pkg/latticeconfig.Env`; bind is comma-separated
  `LATTICE_MODELREPO_BIND` (loopback for the Pi's own work + the tailnet
  address for the gateways) with a boot-race retry, mirroring the frontend.

## 2. Store layout

```
blob-llm/
  blobs/<64-hex-sha256>              content-addressed payloads (both ecosystems)
  blobs/<64-hex-sha256>.part         upload staging (removed on finalize)
  ollama/manifests/registry.ollama.ai/library/<name>/<tag>   raw manifest passthrough
  hf/<bundle>/index.json             per-file sha256 + size + relative path
  hf/<bundle>/files/<relative-path>
```

## 3. HTTP API

All routes emit exactly one telemetry line per request.

| Route | Verbs | Behavior |
|---|---|---|
| `/health` | GET | `{"ok":true,"store":…,"repo":…,"free_bytes":…}` (statfs) |
| `/v1/blobs` | GET | list `{sha256,size}` of finalized blobs |
| `/v1/blobs/{sha}` | HEAD, GET | 200 + Content-Length; GET honors `?offset=N`; 404 unknown |
| `/v1/blobs/{sha}` | PUT | stream stage into `<sha>.part` (O_TRUNC, or `?part=<offset>` appends/seek) |
| `/v1/blobs/{sha}/finalize` | POST | streaming sha256 over the part; match → rename to final; mismatch → 400 and the part is removed; already-final → 409; no part → 404 |
| `/v1/ollama/manifests/{path…}` | GET, PUT | raw passthrough (safeRel-validated) |
| `/v1/hf/{bundle}/index.json`, `/v1/hf/{bundle}/files/{rel}` | GET, PUT | bundle passthrough, atomic rename on write |

There is **no DELETE** — the service never deletes anything. Eviction is a
human `rm` on the client host, run only after a verified push (runbook).

A blob name is only earned by content: `finalize` verifies the sha256 of the
bytes on the wire before the rename, so the store cannot hold data under a
name it does not match.

## 4. Sync client

`deploy/modelrepo-sync.sh` (run on a Mac):

- `push-ollama <model>[:<tag>]` — walks the manifest's digests, pushes each
  blob (HEAD-then-PUT; already-present blobs skip), then pushes the manifest
  itself as a blob and through the raw manifest passthrough.
- `restore-ollama <model>[:<tag>]` — fetches the manifest, downloads missing
  blobs, **verifies each with `shasum -a 256`** against the digest before
  considering it restored.
- `push-hf <bundle>` / `restore-hf <bundle>` — same discipline over the HF
  cache's model directories (safetensors/gguf/json + generated `index.json`).
- `verify <sha>` — HEAD a blob.

Large uploads use `curl -T` (streaming PUT); `--data-binary @file` buffers
the whole file and dies at ~1 GB.

## 5. Deployment

- Systemd **user** unit `deploy/lattice-modelrepo.service` under the Pi's
  lattice service account (`EnvironmentFile=%h/.config/lattice/…`, Restart=always).
- Env example with placeholders only (invariant 8): the env carries the bind
  (loopback + tailnet), the store volume, the telemetry path.
- The telemetry file under `/var/log/lattice/` must be pre-created owned by
  the service account (dir is root-owned; the open fails silently by design
  and the row is dropped otherwise — match the other planes).

## 6. Invariant check

1. New bindings: store on loopback + tailnet only, never a wildcard; the
   frontend/proxy chain is untouched.
2. Pi stores bytes, Macs compute — the repo hashes and renames, nothing more.
3. Protocol surface untouched; blob/manifest routes are store-internal.
4. No decisions in the store; eviction decisions are a human's (runbook).
5. Flat files, no new infra (no DB, no queue).
6. Bee runtime untouched; `bee-feed-lattice.sh` gains one tail-list line if
   the modelrepo telemetry plane is fed.
7. Push + server-side verify + restore-roundtrip precede every `rm` (runbook);
   the benchmark's swap Δ gate is unaffected.
8. Committed files carry placeholders and `%h` only.
9. `request_id` correlation on every modelrepo telemetry row.

## 7. Acceptance

- `go test ./cmd/lattice-modelrepo/` — content-addressing, mismatched-upload
  rejection, offset resume, finalize conflict, manifest/HF passthrough,
  path-traversal rejection, telemetry on both exit paths.
- `/health` answers on loopback and from the Mac gateways over the tailnet.
- bge-m3 (1.2 GB) pushed from a Mac, HEAD-verifiable, restored to a scratch
  directory with matching sha256.
- First real eviction (the 30b+ tier, ~55 GB) reclaims local disk on the Mac
  with a verified, re-restorable server copy.