# Spec: Apple Acceleration Stack — MLX, Metal, ANE/CoreML, Neural Accelerators

**Status:** Scoping (future reference)
**Host:** Mac (Lattice Gateway)
**Gate:** M6 (32 GB unified memory) arrival

This document records the investigation into Apple's inference-acceleration
stack and what each layer means for Lattice's local (Mac) inference path. It is
written for *future reference* — the decisions it scopes are gated on hardware
that has not arrived yet. The concrete measurement behind P1 lives in
[`docs/measurements.md`](../measurements.md).

## 1. Why this matters

The Mac is the **only** host permitted to compute local models — the RPi4 and
RPi3 are cloud-only and must request inference from the Mac. So the Mac's
inference speed *is* Lattice's local ceiling. Today that ceiling is set by
Ollama (which wraps llama.cpp) on an Apple M1 with 16 GB. An **M6 with 32 GB**
is arriving, which raises both the memory ceiling and — via new silicon — the
compute ceiling. The question is which Apple frameworks Lattice should adopt to
harvest the new headroom, and which are dead ends.

## 2. The stack, layer by layer

Apple exposes four distinct acceleration layers. They are not interchangeable:
each targets a different part of the transformer workload.

### 2.1 Metal / MPS — already in use

Metal is the GPU compute API; Metal Performance Shaders (MPS) is its kernel
library. llama.cpp — and therefore Ollama — already runs on Metal, which is
why Lattice's local path works at all. No action: this is the baseline.

### 2.2 MLX — the measurable win (P1)

MLX is Apple's machine-learning framework: a NumPy-like array API over
unified memory with no CPU↔GPU copies, plus `mlx-lm` for LLM inference.
Because it is Apple-native it schedules Metal kernels more tightly than
llama.cpp's generic Metal backend.

**Measured 2026-09-29 (see measurements.md):** on the M1, MLX decodes
**~43 % faster** than llama.cpp at the same 4-bit width (13.3 vs 9.3 tok/s),
same resident memory (~1.9 GB), no swap movement. The win is a runtime
property — decode is memory-bandwidth-bound, and MLX's memory path is tighter.

**The catch:** MLX serves one request at a time (no continuous batching) and
is a Python library, not a bundled server — using it means running an MLX
process on the Mac *alongside* Ollama. On 16 GB that is two resident runtimes;
on 32 GB it is comfortable.

### 2.3 Neural Accelerators — the prefill win (P4)

This is what "MTP / Metal M-chip neural accelerators" refers to: the **M5 and
M6 GPU put a dedicated matrix-multiply engine (a "Neural Accelerator", Apple's
term for an on-die tensor core) inside every shader core**. They are exposed
through the **Metal 4 tensor API** and **MPP TensorOps**, not through a
capability flag.

They accelerate the **compute-bound** phase of a transformer:

- `matmul2d` at SIMD-group / threadgroup scope;
- **cooperative tensors** — register-resident, so attention intermediates are
  updated in place without a memory round-trip;
- quantized dtypes: 4/8-bit int and fp, 2-bit int, and MX/FP8 block scales.

FlashAttention is built directly on this: a Q×K `matmul2d` into a cooperative
tensor, a row-wise SoftMax reduce, and direct reuse of the tensor for the P×V
matmul. The target is **prefill** (prompt processing) and the feed-forward
GEMMs, where arithmetic intensity is high.

**Independent measurement (BaseRT, M5 Pro):** up to **6.4×** faster prefill
than llama.cpp and **3.9×** than MLX — largest on mixture-of-experts models,
where matmul dominates. Apple's own figures: time-to-first-token up to **4×**
faster, token generation up to **25 %** faster on M5 vs M1.

**The key insight for Lattice:** decode (token generation) is
bandwidth-bound, so the Neural Accelerators give *nothing* there — BaseRT's
decode gain was ~1.75× and came from kernel fusion, not the tensor cores. The
Neural Accelerators buy **prefill speed (TTFT)**, which is exactly what a
routing layer cares about for `interactive` requests with long prompts.

### 2.4 Neural Engine / ANE / CoreML — rejected for LLM (P3)

The **Neural Engine** (ANE) is a separate fixed-function block, and **CoreML**
is the framework that targets it. The ANE is convolution-oriented and tuned for
the small, always-on models of Apple Intelligence (image, audio, tiny text).

For LLM decode it is the wrong tool: **2–5× slower** than the GPU at
transformer decode, and the LLM weights do not fit its buffers. Its one
virtue is ~80× better power efficiency, which matters for always-on tasks, not
for a one-shot inference router.

**Verdict: do not pursue ANE/CoreML for Lattice.** The M6's "Dual 16-core
Neural Engine" is advertised for Apple Intelligence workloads, not for hosting
the routing models. CoreML's one plausible use — running a *tiny* on-device
routing classifier — is out of scope for Lattice, whose routing is policy, not
a learned model.

## 3. The M6, specifically

The M6 is Apple's first **2 nm** chip (TSMC N2, gate-all-around nanosheet
transistors). The points that change Lattice's calculus:

| spec | value | relevance |
|---|---|---|
| CPU | 12-core (2 super + 4 perf + 6 efficiency) | fastest single-thread; feeds prompt processing |
| GPU | 12-core, **Neural Accelerator per core** | prefill/TTFT win (P4) |
| Neural Engine | **Dual 16-core** (2× peak) | Apple Intelligence only — not for Lattice |
| unified memory | **up to 32 GB** | the headroom the 20 B MoE needed (see measurements.md) |
| memory bandwidth | up to **170 GB/s** (24/32 GB configs) | decode is bandwidth-bound — this is the decode lever |
| process | 2 nm | efficiency |

Two read-offs:

1. **32 GB unlocks the models that 16 GB rejected.** `gpt-oss:20b` (14.4 GB)
   and a 20 B MoE need 32 GB+ — the M6 is exactly the host the MoE/mmap
   measurements said was required (measurements.md, "num_gpu:0 re-test").
2. **Bandwidth, not compute, sets decode.** 170 GB/s (vs the M1's ~68 GB/s) is
   ~2.5× more bandwidth, which is why decode speed will roughly 2.5× even
   *without* the Neural Accelerators; the Neural Accelerators then stack on top
   for prefill.

## 4. Scoped proposals

### P1 — benchmark MLX vs Ollama on the M1 — DONE

Result: MLX **~43 % faster decode**, same memory. Recorded in measurements.md.

### P2 — MLX provider (conditional, now justified)

P1 cleared the gate (well over the 10 % threshold). The decision is not "is MLX
faster" (yes) but "when and where to run it":

- **Defer to the M6.** On 16 GB, running MLX alongside Ollama is two resident
  runtimes; on 32 GB it is comfortable. The 43 % decode win is real but small
  next to the ~2.5× the M6's bandwidth gives for free.
- **Provider shape.** MLX would plug into the existing `Provider` abstraction
  as an HTTP endpoint (an `mlx-lm` server or `vllm-mlx`), not embedded code —
  MLX is Python/Metal and cannot run inside the Go gateway. The gateway's
  `providers` registry is currently hardcoded to `"ollama"`; adding MLX is a
  second endpoint of the same shape.
- **Open question before building:** does MLX expose the M6 Neural Accelerators
  yet? If not, the MLX win on the M6 is *only* the decode-side runtime win,
  while prefill needs the Metal tensor API directly (P4).

### P3 — ANE / CoreML — REJECTED

Fixed-function Neural Engine is 2–5× slower at LLM decode and cannot host the
routing models. No Lattice action; revisit only if a learned routing
classifier (not policy routing) is ever proposed.

### P4 — M6 Neural Accelerators — WATCH / scope for prefill

The per-GPU-core Neural Accelerators (Metal 4 tensor API) are the M6's real
novelty and target **prefill**, which Lattice's `interactive` path cares about.
But they require writing kernels against the Metal tensor API — that is
llama.cpp's and MLX's job to adopt, not Lattice's. The realistic path:

1. **Watch llama.cpp / MLX** for Metal-4-tensor adoption; Ollama inherits it
   free, MLX may expose it as a flag.
2. **Re-measure prefill (TTFT)** on the M6 once it arrives, at the same
   protocol as measurements.md, for a long-prompt `interactive` request.
3. **Only then** decide whether a prefill-optimised local path is worth a
   second provider. The likely answer is that Ollama's llama.cpp adopts the
   tensor cores upstream and Lattice gets the prefill win without writing a
   line of Metal.

## 5. Decision log (for the next session)

- **P1:** MLX 13.3 tok/s vs llama.cpp 9.3 tok/s on M1 (43 % decode win). Gate
  cleared.
- **P2:** justify MLX *provider*, but defer the actual second runtime to the
  M6/32 GB — two runtimes on 16 GB is memory-risky.
- **P3:** ANE/CoreML rejected for LLM; fixed-function, conv-oriented, 2–5×
  slower at decode.
- **P4:** Neural Accelerators buy prefill/TTFT only; harvest via upstream
  llama.cpp/MLX adoption, re-measure TTFT on M6 before any custom work.

## Sources

- [Apple Newsroom — M6 and M5 Ultra (Aug 2026)](https://www.apple.com/newsroom/2026/08/apple-introduces-m6-and-m5-ultra-for-a-big-leap-in-performance-and-ai-compute/)
- [MacRumors — M5 vs M6 buyer's guide](https://www.macrumors.com/guide/m5-vs-m6/)
- [Wikipedia — Apple M6](https://en.wikipedia.org/wiki/Apple_M6)
- [Apple WWDC26 — Optimize custom ML ops with Metal tensors](https://developer.apple.com/videos/play/wwdc2026/330/)
- [Apple Tech Talk — Accelerate ML with the M5 and A19 GPUs](https://developer.apple.com/videos/play/tech-talks/111432/)
- [arXiv:2607.19438 — BaseRT: LLM inference with M5 Neural Accelerators](https://arxiv.org/html/2607.19438)
