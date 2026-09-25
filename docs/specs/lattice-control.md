# Spec: Lattice Control (Phase 2)

**Status:** Draft
**Host:** RPi4 (`user@rpi4`)

The Lattice Control plane is the intelligence of the system. It does not execute inference; it performs the **routing decision**.

## 1. Core Responsibilities

1. **Routing Decision**: Map an `inference.v1` request to a specific execution target (Mac Gateway or Cloud Provider).
2. **Capability Registry**: Track which models are resident on the Mac and which are available in the cloud.
3. **Concurrency Gating**: Enforce the 3-parallel cap on cloud requests.
4. **Health Monitoring**: Detect if the Mac Gateway is down and trigger fallback to cloud.

## 2. Routing Algorithm

When a request arrives at the Control plane:

1. **Privacy Gate** (Hard):
   - If `privacy == LOCAL_ONLY` and Mac Gateway is unhealthy $\rightarrow$ return `503 Service Unavailable` (Fail Closed).
2. **Latency/Resource Path**:
   - If `latency_class == interactive`:
     - Attempt route to **Cloud**.
     - Check Cloud Concurrency: If current parallel cloud calls $\ge 3$, queue the request or spill to **Local (Mac)** if privacy allows.
   - If `latency_class == batch`:
     - Route to **Local (Mac)**.
3. **Fallback**:
   - If target is Mac and Mac is unhealthy $\rightarrow$ route to Cloud (if privacy allows).
4. **Decision**:
   - Return the Target Endpoint + Model Mapping.

## 3. Capability Registry

A simple map of model aliases to providers:
- `local-brain` $\rightarrow$ Mac Gateway (`granite4:3b`)
- `cloud-brain` $\rightarrow$ rpi4 Cloud (`gemma4:31b-cloud`)
- `local-coder` $\rightarrow$ Mac Gateway (`hermes3:8b`)
- `cloud-coder` $\rightarrow$ rpi4 Cloud (`deepseek-v4-pro:cloud`)

## 4. Concurrency Management

The Control plane maintains a counter for active cloud requests.
- `Increment` on dispatch to cloud.
- `Decrement` on response from cloud.
- Use a simple Go channel or atomic counter.

## 5. Implementation Plan

- **Language**: Go (Single binary).
- **API**: A simple HTTP server that acts as the "Lattice Frontend".
- **State**: In-memory for concurrency and health; simple JSON file for capability registry.
- **Exit Test**:
  - Send an `interactive` request $\rightarrow$ Route to Cloud.
  - Send a `batch` request $\rightarrow$ Route to Mac.
  - Send 4 concurrent `interactive` requests $\rightarrow$ 3 go to Cloud, 1 is queued/spilled.
  - Mock Mac down $\rightarrow$ Route `LOCAL_PREFERRED` to Cloud.
