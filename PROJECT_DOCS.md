# Project Documentation: Lattice

## 1. System Design
Lattice implements a "Control Plane / Data Plane" split.
- **Control Plane**: Stateless routing logic. Maps (Model Alias + Metadata) $\rightarrow$ (Endpoint + Model Name).
- **Data Plane**: The Gateway. Handles the actual HTTP proxying to Ollama and manages the `localSemaphore` to prevent swap thrashing.

## 2. Hardware Mapping
- **Control Host**: RPi4 (Debian 13). Port `:8082`.
- **Gateway Host**: Mac M1 (macOS). Port `:8081`.
- **Frontend Host**: RPi4 (Debian 13). Port `:8080`.
- **Local Inference**: Ollama on Mac (`:11434`).
- **Cloud Inference**: Ollama Cloud on RPi4 (`:11434`).

## 3. Critical Constraints
- **SSD Health**: The Mac Gateway must not allow enough concurrent requests to force the OS into swap.
- **Cloud Budget**: Max 3 concurrent cloud requests.
- **Sovereignty**: `LOCAL_ONLY` $\rightarrow$ Local Gateway. If Gateway is down, return 503. Never fallback to cloud.

## 4. Deployment Sequence
1. **Mac Gateway**: `go build` $\rightarrow$ Install as service $\rightarrow$ Ensure Ollama is running.
2. **RPi4 Control**: Cross-compile `arm64` $\rightarrow$ Install as systemd service.
3. **RPi4 Frontend**: Cross-compile `arm64` $\rightarrow$ Install as systemd service.

## 5. Operational Maintenance
- **Telemetry**: Logs are written to `/var/log/lattice/telemetry-control.jsonl`.
- **Analysis**: Use `cmd/lattice-stats/main.go` to analyze routing distributions.
- **Health**: Control plane probes Gateway `/health` every 10s.
