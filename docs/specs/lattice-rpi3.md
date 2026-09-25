# Spec: RPi3 Integration (Phase 6)

**Status:** Draft
**Host:** RPi3

The RPi3 is the network's security bastion. It must have access to the Lattice's inference capabilities without compromising its status as a security appliance (i.e., it does not run the control plane or the gateway).

## 1. Integration Model

The RPi3 acts as a standard **Lattice Client**.

1. **Request Path**: RPi3 $\rightarrow$ Lattice Frontend (on RPi4) $\rightarrow$ Control $\rightarrow$ Gateway/Cloud.
2. **Authentication**: Use the existing tailnet trust (rpi3 can reach rpi4).
3. **Connectivity**: Ensure the Lattice Frontend is listening on the tailnet IP, not just `localhost`.

## 2. Implementation Plan

1. **Frontend Networking**: Update the Lattice Frontend to listen on the RPi4's tailnet address (`<tailnet-address>`).
2. **Client Setup**: Create a simple Go client tool on RPi3 (`lattice-cli`) that sends `inference.v1` requests to the Frontend.
3. **Security Audit**: Confirm that RPi3 has NO direct access to the Gateway or the Cloud provider (all traffic must pass through the Control plane's routing).

## 3. Exit Test (Phase 6)

- Run `lattice-cli` on RPi3.
- Send a request for `local-brain` with `LOCAL_PREFERRED`.
- Verify the request is routed through the Frontend and returns a result from the Mac Gateway.
- Verify the request is routed to Cloud if the Mac is down.
