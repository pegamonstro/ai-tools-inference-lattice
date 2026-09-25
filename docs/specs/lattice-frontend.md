# Spec: Lattice Frontend (Phase 4)

**Status:** Draft
**Host:** RPi4

The Lattice Frontend is the single entry point for all inference requests in the homelab. It hides the complexity of the routing decision and the target execution from the client.

## 1. Request Flow

1. **Client** $\rightarrow$ **Lattice Frontend** (`:8080/v1/chat/completions`).
2. **Frontend** $\rightarrow$ **Control Plane** (`:8082/route`) $\rightarrow$ **Decision**.
3. **Frontend** $\rightarrow$ **Target** (Mac Gateway `:8081` or Cloud `:11434`) $\rightarrow$ **Execution**.
4. **Target** $\rightarrow$ **Frontend** $\rightarrow$ **Client**.

## 2. Implementation Logic

The Frontend is a thin proxy:

- **Step 1**: For every request, call the Control Plane `/route` endpoint.
- **Step 2**: Receive the `Decision` (`target`, `endpoint`, `model_name`).
- **Step 3**: Rewrite the request:
  - Set the target URL to `endpoint + "/v1/chat/completions"`.
  - Update the `model` field in the JSON body to `model_name`.
- **Step 4**: Forward the request and return the response.

## 3. Exit Test (Phase 4)

- Client sends a single request to `:8080/v1/chat/completions`.
- The request is routed according to the `routing` metadata.
- The user receives the correct LLM response without knowing if it was cloud or local.
- Telemetry confirms the path: Client $\rightarrow$ Frontend $\rightarrow$ Control $\rightarrow$ Gateway/Cloud.
