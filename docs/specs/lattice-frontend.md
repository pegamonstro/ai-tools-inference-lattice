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

The Frontend is a thin proxy. It **forwards the client's JSON body unchanged**
and rewrites only what routing requires:

- **Step 1**: For every request, call the Control Plane `/route` endpoint. Assign
  the request id first (§2.3), so Control logs the same id the other two planes
  will use.
- **Step 2**: Receive the `Decision` (`target`, `endpoint`, `model_name`).
- **Step 3**: Rewrite the request — exactly two edits, nothing else:
  - Set the target URL to `endpoint + "/v1/chat/completions"`.
  - Update the `model` field in the JSON body to `model_name`.
  - Delete any client-supplied `routing`, then re-inject the frontend-issued
    envelope **only on the local path**: cloud endpoints speak plain OpenAI and
    reject it.
- **Step 4**: Forward the request and return the response.

**Every other field passes through untouched** — `tools`, `tool_choice`,
`temperature`, `max_tokens`, `stop`, and anything a future client adds. The
proxy's default is *allow unless rewritten*.

> **Rule.** The frontend may not enumerate the fields a client is allowed to
> send. Enumerating *is* the bug this rule exists to prevent: an earlier version
> rebuilt the body from `model`, `messages`, and `stream` only, and every other
> field — `tools` among them — was silently dropped. A whitelist at least names
> what it discards; a struct decode does not. Only passthrough loses nothing. See
> the [handbook](../handbook/architecture-handbook.md) §11.

### 2.1 Model resolution

The `model` field is either a **capability alias** (`local-brain`, `local-coder`),
which the Control plane resolves per target, or **any other string**, which is
passed through verbatim as `model_name`. Resolution happens in Control — see
[`lattice-control.md`](lattice-control.md) §3.1 — and `model_name` is never
empty.

### 2.2 Discovery endpoints

Two read-only routes exist so a client that probes before it calls is answered:

- **`GET /v1/models`** — the client-facing namespace: the capability aliases,
  each carrying a `context_length` equal to the ceiling the gateway will honour
  (read from Control's `/capabilities`). This is the namespace clients are
  *expected* to use, so it is the namespace it advertises.
- **`GET /health`** — frontend liveness. Cheap, and the natural probe target.

Both exist because a service that answers on only one route is
indistinguishable from a non-existent one to any client that probes first.

### 2.3 Correlation

Every response carries an **`X-Request-Id`** header. The value is the client's
`routing.request_id` when it supplied one, and a generated id otherwise, so the
key is never blank. It is assigned **before** the Control call, so the same id
appears in all three telemetry streams.

## 3. Exit Test (Phase 4)

- Client sends a single request to `:8080/v1/chat/completions`.
- The request is routed according to the `routing` metadata.
- The user receives the correct LLM response without knowing if it was cloud or local.
- Telemetry confirms the path: Client $\rightarrow$ Frontend $\rightarrow$ Control $\rightarrow$ Gateway/Cloud.
