#!/usr/bin/env bash
# round8-battery.sh — round-8 small-model battery (runs ON the M6).
# Protocol per docs/benchmarks-m6.md: num_ctx 2048, num_predict 128,
# stream=false, warm decode = eval_count/eval_duration on the 2nd call,
# size_vram from /api/ps, swap delta from vm.swapusage.
# Phases per model: cold / warm / swap / native tool / gateway gate (OpenAI
# shape + tools against FRONTEND, default the local gateway) / 3 coding
# cases. Raw JSON kept under /tmp/round8 for post-hoc scoring; human verdicts
# happen off-host.
set -u
OUT=/tmp/round8
mkdir -p "$OUT"
OLLAMA=${OLLAMA:-http://127.0.0.1:11434}
FRONTEND=${FRONTEND:-http://127.0.0.1:8081}
CTX=2048
PRED=128
CODEPRED=256
CHAT_PROMPT="In a few sentences, explain the tradeoffs between Mixture-of-Experts and dense transformer architectures for local inference on memory-constrained hardware."

swap_used_mb() {
  sysctl -n vm.swapusage | sed -E 's/.*used *= *([0-9.]+)([MG]).*/\1 \2/' | awk '{print ($2=="G"?$1*1024:$1)}'
}

jget() { grep -o "\"$2\":[0-9.]*" "$1" | head -1 | cut -d: -f2 | tr -d '.'; }

generate() { # model outfile prompt predict
  curl -s -m 300 "$OLLAMA/api/generate" -d '{
    "model":"'"$1"'","prompt":"'"$3"'","stream":false,
    "think":false,
    "keep_alive":"15m",
    "options":{"num_ctx":'"$CTX"',"num_predict":'"$4"'}
  }' -o "$2"
}

unload() { curl -s -m 20 "$OLLAMA/api/generate" -d '{"model":"'"$1"'","keep_alive":0}' >/dev/null; }

TOOLBODY='"messages":[{"role":"user","content":"What is the weather in Paris right now? Use the provided tool."}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the current weather for a city","parameters":{"type":"object","properties":{"location":{"type":"string","description":"City name"}},"required":["location"]}}}],"stream":false'

for M in "$@"; do
  F="$OUT/$M.log"
  : > "$F"
  echo "=== $M $(date +%H:%M:%S) ===" | tee -a "$F"

  swap0=$(swap_used_mb)
  unload "$M"

  # --- cold: fixed chat prompt, includes load ---
  generate "$M" "$OUT/$M.cold.json" "$CHAT_PROMPT" "$PRED"
  ld=$(jget "$OUT/$M.cold.json" load_duration)
  ec=$(jget "$OUT/$M.cold.json" eval_count)
  ed=$(jget "$OUT/$M.cold.json" eval_duration)
  echo "cold: load=${ld}ns eval=$ec tok in ${ed}ns" >> "$F"

  # --- warm: same prompt again, prompt-cache hit ---
  generate "$M" "$OUT/$M.warm.json" "$CHAT_PROMPT" "$PRED"
  pec=$(jget "$OUT/$M.warm.json" prompt_eval_count)
  ped=$(jget "$OUT/$M.warm.json" prompt_eval_duration)
  wec=$(jget "$OUT/$M.warm.json" eval_count)
  wed=$(jget "$OUT/$M.warm.json" eval_duration)
  echo "warm: prompt_eval=$pec tok in ${ped}ns eval=$wec tok in ${wed}ns" >> "$F"

  vram=$(curl -s -m 10 "$OLLAMA/api/ps" | python3 -c "import json,sys
ps=json.load(sys.stdin)
print([m.get('size_vram','') for m in ps['models'] if m['name']=='$M'][0])")
  swap1=$(swap_used_mb)
  echo "vram=${vram}B swap:${swap0}MB -> ${swap1}MB" >> "$F"

  # --- native tool-call (direct /api/chat) ---
  curl -s -m 300 "$OLLAMA/api/chat" -d '{"model":"'"$M"'",'"$TOOLBODY"',"keep_alive":"15m"}' -o "$OUT/$M.toolnative.json"
  echo "toolnative bytes=$(wc -c < "$OUT/$M.toolnative.json")" >> "$F"

  # --- gateway gate: OpenAI shape + tools against the gateway ---
  curl -s -m 300 "$FRONTEND/v1/chat/completions" -H 'Content-Type: application/json' \
    -d '{"model":"'"$M"'","messages":[{"role":"user","content":"What is the weather in Paris right now? Use the provided tool."}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the current weather for a city","parameters":{"type":"object","properties":{"location":{"type":"string","description":"City name"}},"required":["location"]}}}]}' \
    -o "$OUT/$M.toolgw.json"
  echo "toolgw bytes=$(wc -c < "$OUT/$M.toolgw.json")" >> "$F"

  # --- coding: 3 cases at num_predict 256 ---
  generate "$M" "$OUT/$M.code1.json" "Fix the off-by-one bug in this Python loop and explain in one sentence: for i in range(len(items)): print(items[i+1])" "$CODEPRED"
  echo "code1: $(python3 -c "import json;print(json.load(open('$OUT/$M.code1.json'))['response'][:280].replace(chr(10),' | '))")" >> "$F"
  generate "$M" "$OUT/$M.code2.json" "Write a Python function is_palindrome(s) that ignores case and non-alphanumeric characters. Code only." "$CODEPRED"
  echo "code2: $(python3 -c "import json;print(json.load(open('$OUT/$M.code2.json'))['response'][:280].replace(chr(10),' | '))")" >> "$F"
  generate "$M" "$OUT/$M.code3.json" "This Go code panics for short input: first := s[1:len(s)-1]. Fix it in one sentence." "$CODEPRED"
  echo "code3: $(python3 -c "import json;print(json.load(open('$OUT/$M.code3.json'))['response'][:280].replace(chr(10),' | '))")" >> "$F"

  unload "$M"
  echo "=== $M done ===" | tee -a "$F"
  sleep 2
done
echo "BATTERY COMPLETE $(date +%H:%M:%S)"