# Manual: Model eviction & restore (lattice-modelrepo)

The repo is a dumb, verified byte store. Everything dangerous lives in this
runbook and in your hands. **The golden rule: never `rm` a local model file
before its server copy is verified — HEAD 200 with the right size on final
blobs.** The service has no DELETE endpoint and never touches a Mac's model
directories; eviction is only ever a human running `rm` here.

## 1. Set the target once

Run these from the Mac whose disk you are freeing:

```bash
export REPO_URL=http://<pi-tailnet-ip>:8090   # the lattice-modelrepo bound to the tailnet
```

## 2. Eviction procedure (Ollama models)

### 2a. Push + verify

```bash
bash deploy/modelrepo-sync.sh push-ollama <name>:<tag>
```

The script walks the manifest, pushes every blob (skipping what the server
already holds), pushes the manifest itself, and re-verifies nothing is left
partial. Confirm the server copy before touching the local one:

```bash
# every digest in the manifest must HEAD as 200
for d in $(grep -o '"digest":"[^"]*"' ~/.ollama/models/manifests/registry.ollama.ai/library/<name>/<tag> | sed 's/.*://' | tr -d '"'); do
  curl -sI "$REPO_URL/v1/blobs/$d" -o /dev/null -w "$d %{http_code}\n"
done
```

All lines must read `200`.

### 2b. Evict (local rm)

```bash
# blobs referenced ONLY by this manifest and its now-verified copies
# (a digest may be shared with other models — skip those, or check with
#  grep -rl <hash> ~/.ollama/models/manifests/ first)
for d in $(grep -o '"digest":"[^"]*"' ~/.ollama/models/manifests/registry.ollama.ai/library/<name>/<tag> | sed 's/.*://' | tr -d '"'); do
  grep -rq "$d" ~/.ollama/models/manifests/registry.ollama.ai/library/ --include='*' -r \
    --exclude-dir="$(dirname ~/.ollama/models/manifests/registry.ollama.ai/library/<name>/<tag>)" \
    || rm ~/.ollama/models/blobs/sha256-"$d"
done
rm ~/.ollama/models/manifests/registry.ollama.ai/library/<name>/<tag>
```

In practice: for large evictions check shared digests once by hand, then
delete. Ollama will show the model gone in `/api/tags` after a refresh; a
running loaded copy disappears on its next keep-alive expiry.

### 2c. Prove restore works (do this the same day)

```bash
mkdir -p /tmp/restore-try && OLLAMA_DIR=/tmp/restore-try bash deploy/modelrepo-sync.sh restore-ollama <name>:<tag>
```

`restore-ollama` re-downloads missing blobs and verifies each with
`shasum -a 256` against the manifest digest before declaring success. Delete
the scratch dir after.

## 3. Eviction procedure (HF / mflux bundles)

```bash
bash deploy/modelrepo-sync.sh push-hf models--<bundle-path>       # pushes safetensors/gguf/json + index.json
curl -s "$REPO_URL/v1/hf/models--<bundle-path>/index.json" | python3 -m json.tool   # eyeball the shas
for rel in $(python3 -c "import json;print('\n'.join(f['path'] for f in json.load(open('/dev/stdin'))['files']))" < <(curl -s "$REPO_URL/v1/hf/models--<bundle-path>/index.json")); do
  shasum -a 256 ~/.cache/huggingface/hub/models--<bundle-path>/"$rel"   # must match the index
done
rm -rf ~/.cache/huggingface/hub/models--<bundle-path>             # only after the shas match
```

mflux model dirs (`$HOME/Library/Caches/mflux/...` or bespoke paths the sidecar
configs point at) follow the same shape: `push-hf` the directory, verify the
index, then `rm -rf` the directory.

## 4. Restore for real

```bash
OLLAMA_DIR=~/.ollama/models bash deploy/modelrepo-sync.sh restore-ollama <name>:<tag>
OLLAMA_DIR=~/.cache/huggingface/hub bash deploy/modelrepo-sync.sh restore-hf models--<bundle-path>
```

Restored blobs land under the exact original store paths; Ollama picks models
up on the next request (no daemon restart needed), HF caches on the next run.

## 5. Operating notes

- **Link speed:** bulk pushes/redirects over the tailnet measured ~9 MB/s via
  DERelay and can take hours for a 30b-class model. Before a big push, check
  that the tailnet connection to the Pi is direct (not relayed) — the store
  itself hashes at Pi-disk speed (~6 MB/s effective for a 1.1 GB finalize),
  so plan around minutes-per-GB, not seconds.
- **Receipts:** every operation lands one JSONL row in the store's telemetry
  plane with a `request_id` — the HEAD-200 row is your verified-push receipt.
- **Disk space check on the store side:** `curl -s $REPO_URL/health` reports
  `free_bytes`. The store partition starts with ~690 GB free.
- **Never** push from a script that also deletes; the two steps are separate
  human actions on purpose.
- The first real eviction wave = the 30b+ tier rejected by benchmark round 8
  (~55 GB back on the gate Mac). See `docs/benchmarks-m6.md` round 8 for the
  verdict list.