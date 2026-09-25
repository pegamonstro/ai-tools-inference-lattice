# 🌐 Lattice: Sovereign Inference Orchestration

Lattice is a lightweight, distributed control and execution system designed for homelabs to manage LLM inference across asymmetric hardware (e.g., an Apple Silicon Mac and Raspberry Pis).

## 🎯 The Core Problem
Local LLMs are free and private but can be painfully slow (especially with long contexts or model-swapping). Cloud LLMs are fast but expensive and breach local sovereignty.

Lattice solves this by separating **Decision** from **Execution**.

## 🏗️ Architecture

- **Lattice Control (RPi4)**: The "Brain." It decides *where* a request should go based on privacy, latency needs, and current resource pressure.
- **Lattice Gateway (Mac)**: The "Muscle." It executes local inference and protects the host hardware from memory oversubscription (SSD swap protection).
- **Lattice Frontend**: The single entry point. Clients send a request with metadata; the Frontend orchestrates the flow through the Control plane to the target.

## 🚀 Key Features

- **Privacy-First Routing**: Hard-gated `LOCAL_ONLY` requests never leave the local network.
- **Latency-Aware Scheduling**: Interactive turns are routed to the cloud for speed; batch jobs are routed to the Mac for cost-efficiency.
- **Hardware Sovereignty**: Prevents SSD wear on the Mac by gating local concurrency based on RAM availability.
- **Cloud Concurrency Gating**: Strictly enforces provider limits (e.g., max 3 parallel cloud calls) to prevent subscription throttling.

## 🛠️ Protocol: `inference.v1`
Lattice extends the OpenAI API by adding a `routing` object to requests:
```json
"routing": {
  "privacy": "LOCAL_ONLY | LOCAL_PREFERRED | CLOUD_ALLOWED",
  "latency_class": "interactive | batch",
  "parallelism": 1,
  "request_id": "uuid"
}
```
