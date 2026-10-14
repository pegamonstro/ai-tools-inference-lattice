#!/usr/bin/env bash
# modelrepo-sync.sh — manual offload/restore of model blobs to the Pi's
# lattice-modelrepo store. Run on a Mac; the store holds bytes, this script
# drives them. Everything here is human-invoked: nothing deletes local model
# files (eviction `rm`s live in the model-repo runbook, run only AFTER a
# verified push).
#
#   push-ollama <model>[:<tag>]   push one Ollama model's blobs + manifest; verify
#   restore-ollama <model>[:<tag>]  pull back into the local Ollama store
#   push-hf <model-dir-name>      push one HF-cache model dir (models--*) + index
#   restore-hf <model-dir-name>   pull it back
#   verify <sha256>               HEAD a blob on the server
#
# Env:
#   REPO_URL   the lattice-modelrepo base (default http://127.0.0.1:8090)
#   OLLAMA_DIR local Ollama store (default ~/.ollama/models)
#   HF_DIR     local HF cache (default ~/.cache/huggingface/hub)
set -euo pipefail

REPO_URL="${REPO_URL:-http://127.0.0.1:8090}"
OLLAMA_DIR="${OLLAMA_DIR:-$HOME/.ollama/models}"
HF_DIR="${HF_DIR:-$HOME/.cache/huggingface/hub}"

die() { echo "modelrepo-sync: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null || die "missing tool: $1"; }
need curl; need shasum

sha_part() { printf '%s' "$1" | cut -d- -f2; }

# local_blob <bare-hex-sha> — Ollama stores blobs on disk as "sha256-<hex>".
local_blob() { printf '%s/sha256-%s\n' "$OLLAMA_DIR/blobs" "$1"; }

# push_blob <local-file> -> prints server blob name
push_blob() {
  local src="$1" sha
  sha="$(shasum -a 256 "$src" | awk '{print $1}')"
  if [[ "$(curl -s -o /dev/null -w '%{http_code}' -I "$REPO_URL/v1/blobs/$sha")" == "200" ]]; then
    echo "$sha [server-already]"
    return 0
  fi
  curl -sf -T "$src" "$REPO_URL/v1/blobs/$sha" >/dev/null \
    || die "upload failed: $src"
  local finalized; finalized="$(curl -sf -X POST "$REPO_URL/v1/blobs/$sha/finalize")" || die "finalize failed: $sha"
  [[ "$(curl -sI "$REPO_URL/v1/blobs/$sha" -o /dev/null -w '%{http_code}')" == "200" ]] \
    || die "server verify failed HEAD $sha"
  echo "$sha"
}

push_ollama() {
  local model="$1"
  local manifest="$OLLAMA_DIR/manifests/registry.ollama.ai/library/${model%%:*}/${model##*:}"
  if [[ "$model" == *:* ]]; then :; else manifest="$OLLAMA_DIR/manifests/registry.ollama.ai/library/$model/latest"; fi
  [[ -f "$manifest" ]] || die "no local manifest: $model"
  for digest in $(grep -o '"digest":"[^"]*"' "$manifest" | sed 's/.*://' | tr -d '"'); do
    local blob
    blob="$(local_blob "$digest")"
    [[ -f "$blob" ]] || die "missing blob locally: $blob"
    echo "push $(basename "$blob") $(du -h "$blob" | cut -f1)"
    push_blob "$blob" >/dev/null
  done
  push_blob "$manifest" >/dev/null
  curl -sf -T "$manifest" "$REPO_URL/v1/ollama/manifests/registry.ollama.ai/library/${model%%:*}/${model##*:}" >/dev/null
  echo "verified: $model on $REPO_URL"
}

restore_ollama() {
  local model="$1"
  local name="${model%%:*}" tag; tag="${model##*:}"; [[ "$model" == *:* ]] || tag=latest
  local rel="registry.ollama.ai/library/$name/$tag"
  mkdir -p "$OLLAMA_DIR/blobs" "$(dirname "$OLLAMA_DIR/manifests/$rel")"
  curl -sf "$REPO_URL/v1/ollama/manifests/$rel" -o "$OLLAMA_DIR/manifests/$rel" \
    || die "no server manifest: $rel"
  for digest in $(grep -o '"digest":"[^"]*"' "$OLLAMA_DIR/manifests/$rel" | sed 's/.*://' | tr -d '"'); do
    local blob
    blob="$(local_blob "$digest")"
    [[ -f "$blob" ]] && { echo "have $(basename "$blob")"; continue; }
    curl -sf "$REPO_URL/v1/blobs/$digest" -o "$blob" \
      || die "restore failed: $digest"
    local got
    got="$(shasum -a 256 "$blob" | awk '{print $1}')"
    [[ "$digest" == "$got" ]] || die "restored blob sha mismatch: $blob"
    echo "restored $(basename "$blob")"
  done
  echo "restored manifest ($tag -> local store)"
}

push_hf() {
  local dir="$HF_DIR/$1" bundle="$1"
  [[ -d "$dir" ]] || die "no HF dir: $dir"
  local index="$dir/_modelrepo.index.json"
  {
    echo '{"files":['
    local first=1
    while IFS= read -r f; do
      local rel="${f#"$dir"/}" sha size
      sha="$(shasum -a 256 "$f" | awk '{print $1}')"
      size="$(stat -f %z "$f" 2>/dev/null || stat -c %s "$f")"
      (( first )) || echo ","
      printf '{"path":"%s","sha256":"%s","size":%s}' "$rel" "$sha" "$size"
      first=0
    done < <(find "$dir" -type f -name "*.safetensors" -o -type f -name "*.gguf" -o -type f -name "*.json" | sort)
    echo ']}'
  } > "$index"
  while IFS= read -r f; do
    echo "push ${f#"$dir"/}"
    curl -sf -T "$f" "$REPO_URL/v1/hf/${1}/files/${f#"$dir"/}" >/dev/null || die "upload failed: $f"
  done < <(find "$dir" \( -name "*.safetensors" -o -name "*.gguf" -o -name "*.json" \) -type f)
  curl -sf -T "$index" "$REPO_URL/v1/hf/${1}/index.json" >/dev/null
  echo "verified index: $1"
}

restore_hf() {
  local dir="$HF_DIR/$1"
  mkdir -p "$dir"
  curl -sf "$REPO_URL/v1/hf/$1/index.json" -o "$dir/_modelrepo.index.json" || die "no server bundle: $1"
  while IFS= read -r row; do
    local rel sha want
    rel="$(printf '%s' "$row" | sed -n 's/.*"path":"\([^"]*\)".*/\1/p')"
    sha="$(printf '%s' "$row" | sed -n 's/.*"sha256":"\([^"]*\)".*/\1/p')"
    [[ -f "$dir/$rel" && "$(shasum -a 256 "$dir/$rel" | awk '{print $1}')" == "$sha" ]] && { echo "have $rel"; continue; }
    mkdir -p "$(dirname "$dir/$rel")"
    curl -sf "$REPO_URL/v1/hf/$1/files/$rel" -o "$dir/$rel" || die "restore failed: $rel"
    want="$(shasum -a 256 "$dir/$rel" | awk '{print $1}')"
    [[ "$want" == "$sha" ]] || die "restored file sha mismatch: $rel"
    echo "restored $rel"
  done < <(tr ',' '\n' < "$dir/_modelrepo.index.json" | grep '"path"')
}

verify() {
  curl -sI "$REPO_URL/v1/blobs/$1" -o /dev/null -w "HEAD $1 -> %{http_code}\n"
}

case "${1:-}" in
  push-ollama) push_ollama "$2" ;;
  restore-ollama) restore_ollama "$2" ;;
  push-hf) push_hf "$2" ;;
  restore-hf) restore_hf "$2" ;;
  verify) verify "$2" ;;
  *) sed -n '2,20p' "$0"; exit 1 ;;
esac