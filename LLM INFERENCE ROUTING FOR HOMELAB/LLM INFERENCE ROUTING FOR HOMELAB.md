LLM INFERENCE ROUTING FOR HOMELAB  
 ## Hi! I have created a personal and complex layout of LLM inference spread across 3 nodes and using hybrid of cloud and local models. This presents serious challenges to LLM routing through different providers and and different nodes requesting inference.  
How can I ensure it’s proper functioning and is there any FOSS projects that handle this issue and have a solution for it? I am looking at omnirouter and wondering if this could be the LLM router needed to handle the multi-agent complex conditions and workflows that my network/lab of three hosts needs?  
##   
Yes. What you are describing is no longer simply an “LLM router”. You have a **distributed inference control plane**: multiple hosts, local Ollama models, cloud providers, different capabilities, different latency/cost/privacy characteristics, and agents that may themselves generate concurrent inference requests.  
That distinction matters.  
I would **not make OmniRoute the architectural brain** of the system. It may be a useful gateway layer, but for your three-node setup I would put a more deterministic routing/control layer above it—or use LiteLLM as the gateway and keep the actual policy in your own control plane.  
**The architecture I would aim for**  
```
                     ┌──────────────────────────────┐
                     │        Agents / Clients      │
                     │                              │
                     │ Hermes │ SEED │ Codex │ apps │
                     └──────────────┬───────────────┘
                                    │
                                    ▼
                     ┌──────────────────────────────┐
                     │      INFERENCE CONTROL       │
                     │                              │
                     │ capability matching          │
                     │ policy / privacy             │
                     │ task classification          │
                     │ priority / QoS               │
                     │ concurrency / leases         │
                     │ retry / fallback             │
                     │ health / circuit breakers    │
                     │ telemetry / accounting       │
                     └──────────────┬───────────────┘
                                    │
                         logical model request
                                    │
              ┌─────────────────────┼─────────────────────┐
              ▼                     ▼                     ▼
       ┌─────────────┐       ┌─────────────┐       ┌─────────────┐
       │   Node A    │       │   Node B    │       │   Node C    │
       │   local     │       │   local     │       │   cloud     │
       │   Ollama    │       │   Ollama    │       │ providers   │
       └──────┬──────┘       └──────┬──────┘       └──────┬──────┘
              │                     │                     │
              └─────────────────────┼─────────────────────┘
                                    ▼
                            inference result

```
The key idea is that **agents should request capabilities, not infrastructure**.  
For example:  
```
{
  "task": "code_reasoning",
  "context_tokens": 42000,
  "requires": ["tool_calling", "structured_output"],
  "privacy": "local_preferred",
  "latency": "normal",
  "priority": 70,
  "max_cost_usd": 0.05
}

```
The router then determines where that request goes.  
That is much more powerful than:  
```
agent → qwen3.5

```
because the latter hard-codes your infrastructure into every agent.  
  
## Where OmniRoute fits  
OmniRoute is interesting because its current design is explicitly aimed at putting multiple providers behind one endpoint, with automatic fallback and routing “combos”. It supports a large provider catalogue and exposes OpenAI-compatible interfaces. ++[GitHub](https://github.com/artzy/OmniRoute?utm_source=chatgpt.com)++  
So **yes, OmniRoute could be useful in your system**.  
But I would distinguish:  
**OmniRoute = provider gateway/router**  
from  
**your inference control plane = distributed decision system**  
That's an important architectural boundary.  
OmniRoute's auto/combo concept is useful for:  
```
Cloud provider A
      ↓ failure
Cloud provider B
      ↓ quota
Cloud provider C

```
But your problem is broader:  
```
                  ┌─ local model on Node A
                  ├─ local model on Node B
request ──────────┼─ cloud model A
                  ├─ cloud model B
                  └─ cloud model C

```
where the choice might depend on:  
* context length  
* modality  
* tool support  
* structured-output support  
* model quality  
* privacy  
* network availability  
* current GPU/CPU utilisation  
* queue depth  
* latency  
* token throughput  
* provider quota  
* cost  
* task priority  
* agent identity  
* failure state  
* whether the task is allowed to leave your LAN  
That's a **scheduler/policy problem**, not merely provider routing.  
  
## The FOSS projects I would seriously examine  
**1. LiteLLM — probably the most important baseline**  
LiteLLM currently provides a unified gateway for 100+ providers, OpenAI-compatible APIs, virtual keys, logging, load balancing, fallbacks, retries, cooldowns and routing groups. ++[GitHub](https://github.com/isaccanedo/litellm?utm_source=chatgpt.com)++  
This is probably the first thing I would benchmark against your current system.  
Its routing layer already understands concepts such as:  
```
model group
   ├── deployment A
   ├── deployment B
   └── deployment C

```
and can perform failover/load balancing.  
That maps quite naturally onto:  
```
logical model
       │
       ├── Node A / Ollama
       ├── Node B / Ollama
       └── Cloud provider

```
**However:** I wouldn't let LiteLLM become your entire control plane.  
Use it as the **inference gateway/data plane**.  
  
**2. OmniRoute — very interesting for the cloud side**  
OmniRoute is particularly interesting if your problem includes lots of cloud providers and provider-specific failure/quota behaviour.  
Its “combo” abstraction is essentially:  
```
model A
   ↓
provider A
   ↓ failure/quota
provider B
   ↓ failure
provider C

```
and it can expose the resulting system through one endpoint. ++[GitHub](https://github.com/artzy/OmniRoute?utm_source=chatgpt.com)++  
That makes it potentially useful **behind your own router**:  
```
                    YOUR ROUTER
                         │
             ┌───────────┴───────────┐
             │                       │
          LOCAL                  CLOUD
             │                       │
      ┌──────┴──────┐          OmniRoute
      │             │               │
    Node A        Node B       A/B/C/D providers

```
That's actually a configuration I would investigate.  
  
**3. RouteLabs Router**  
There is also a newer open-source project explicitly targeting **local-first hybrid local/cloud routing**.  
Its stated feature set is unusually close to what you're describing: Ollama discovery, local runtimes, cloud fallback, privacy-aware routing, capability matching, tool calling, MCP-aware requests, cost/latency visibility and eventual agent-step routing. ++[GitHub](https://github.com/routelabsai/router?utm_source=chatgpt.com)++  
I'd definitely put this on the experimental shortlist.  
The caveat is maturity. I would not immediately make it the foundation of your infrastructure without testing its behaviour under failure and concurrency.  
  
**4. llmrouter**  
Another interesting lightweight project provides a unified gateway supporting OpenAI, Anthropic, Gemini, Ollama and other providers, protocol translation, context awareness, weighted load balancing and a programmable “smart routing” layer. ++[GitHub](https://github.com/paularlott/llmrouter?utm_source=chatgpt.com)++  
This is conceptually attractive for a homelab because it is closer to:  
```
one logical inference interface
          ↓
provider abstraction
          ↓
routing policy

```
rather than building a huge platform.  
  
**5. Rust litellm-rs**  
Given your preference for low-overhead infrastructure, I would also keep an eye on litellm-rs.  
It is implementing a self-hosted gateway in Rust with OpenAI-compatible APIs, provider routing, load balancing, failover, cost optimisation, authentication, metrics and health endpoints. ++[GitHub](https://github.com/majiayu000/litellm-rs?utm_source=chatgpt.com)++  
For something eventually living on your RPi4-class infrastructure, **Rust is architecturally interesting**.  
  
## The really important part for your three-node system  
I would **not** do this:  
```
Hermes → OmniRoute → Ollama
SEED   → OmniRoute → Ollama
Agent  → OmniRoute → cloud
Codex  → OmniRoute → Ollama

```
because you eventually get several independent routers making decisions.  
Instead:  
```
                    ┌───────────────────┐
                    │ Inference Control │
                    │      Plane        │
                    └─────────┬─────────┘
                              │
                   ┌──────────┴──────────┐
                   │                     │
             LOCAL FABRIC           CLOUD FABRIC
                   │                     │
            ┌──────┴──────┐        OmniRoute/LiteLLM
            │             │               │
          Node A        Node B       providers...

```
**One component owns the routing decision.**  
Everything underneath it is an execution resource.  
  
## And I would make routing capability-based  
This is where your architecture could become substantially more robust.  
Instead of agents knowing:  
```
qwen3.5:cloud
claude-sonnet
qwen2.5-coder

```
they ask:  
```
CAPABILITY_CODE_REASONING
CAPABILITY_VISION
CAPABILITY_LONG_CONTEXT
CAPABILITY_FAST_RESPONSE
CAPABILITY_PRIVATE
CAPABILITY_HIGH_RELIABILITY

```
The router maintains something like:  
```
qwen-local:
  location: node-a
  capabilities:
    - code
    - reasoning
    - structured_output
  privacy: local
  context: 32768
  throughput: 18

qwen-cloud:
  location: cloud
  capabilities:
    - code
    - reasoning
    - vision
    - long_context
  privacy: external
  context: 128000

claude:
  location: cloud
  capabilities:
    - reasoning
    - vision
    - tools
    - long_context
  privacy: external

```
Then the request becomes:  
```
task: reasoning
requires:
  - structured_output

privacy:
  maximum: LAN

priority: 80

```
and the router determines the actual model.  
This means you can replace a model without modifying Hermes, SEED or any other agent.  
  
## The other thing you absolutely need: observability  
Before deciding on OmniRoute/LiteLLM/etc., I would build a **routing test harness**.  
Every inference request should produce a record roughly like:  
```
{
  "request_id": "req-8e21",
  "agent": "hermes",
  "task": "code_reasoning",

  "requested_capabilities": [
    "reasoning",
    "structured_output"
  ],

  "selected_node": "rpi4-107",
  "selected_model": "qwen3.5",

  "queue_ms": 12,
  "ttft_ms": 841,
  "generation_ms": 5320,

  "input_tokens": 12400,
  "output_tokens": 2180,

  "fallbacks": 0,
  "retries": 0,

  "success": true
}

```
Then deliberately break things.  
**Test matrix**  

| Failure                           | Expected behaviour        |
| --------------------------------- | ------------------------- |
| Ollama unavailable                | cloud/local fallback      |
| Node disappears                   | requests migrate          |
| provider timeout                  | retry/fallback            |
| provider rate limit               | alternate deployment      |
| model doesn't support vision      | reject/redirect           |
| context too large                 | select long-context model |
| cloud unavailable                 | local degradation         |
| LAN unavailable                   | defined failure           |
| two agents request simultaneously | deterministic scheduling  |
| 20 concurrent requests            | bounded queue             |
| provider returns malformed output | validation/fallback       |
| router crashes                    | requests fail predictably |
| network partitions                | no split-brain routing    |
  
That test suite is arguably more important than which router you choose.  
  
## For your particular architecture  
Given what I know about the way you've been building **Hermes + S.E.E.D. + the three-node homelab**, I would actually separate the system into three layers:  
```
                 ┌─────────────────────────────┐
                 │        HERMES / S.E.E.D.   │
                 │        cognitive layer     │
                 └──────────────┬──────────────┘
                                │
                       TASK / INTENT
                                │
                                ▼
                 ┌─────────────────────────────┐
                 │   INFERENCE CONTROL PLANE  │
                 │                             │
                 │ policy                      │
                 │ capability resolution       │
                 │ scheduling                  │
                 │ concurrency                 │
                 │ health                       │
                 │ circuit breakers             │
                 │ telemetry                    │
                 └──────────────┬──────────────┘
                                │
                         canonical API
                                │
                ┌───────────────┴───────────────┐
                │                               │
                ▼                               ▼
       ┌─────────────────┐             ┌─────────────────┐
       │ LOCAL GATEWAY   │             │ CLOUD GATEWAY   │
       │ LiteLLM/etc.    │             │ OmniRoute/etc.  │
       └────────┬────────┘             └────────┬────────┘
                │                               │
          Node A / B                       Providers
          Ollama/etc.

```
This is much closer to a **distributed inference fabric** than an LLM router.  
And importantly, it preserves something you've already been doing with Hermes: **the control plane remains deterministic and inspectable rather than allowing an LLM to decide how the infrastructure itself behaves.**  
I would make that a hard architectural rule.  
  
## My current shortlist  

| Project          | Role I'd give it                           |
| ---------------- | ------------------------------------------ |
| LiteLLM          | Primary gateway candidate / baseline       |
| OmniRoute        | Cloud-provider routing/fallback candidate  |
| RouteLabs Router | Experimental hybrid local/cloud candidate  |
| llmrouter        | Lightweight alternative worth benchmarking |
| litellm-rs       | Long-term low-resource/Rust candidate      |
  
++[OmniRoute repository](https://github.com/artzy/OmniRoute?utm_source=chatgpt.com)++ ++[LiteLLM repository](https://github.com/BerriAI/litellm?utm_source=chatgpt.com)++ ++[RouteLabs Router repository](https://github.com/routelabsai/router?utm_source=chatgpt.com)++  
**My architectural instinct**  
**Don't replace your existing inference architecture with OmniRoute yet.**  
Instead, build a small **Inference Control Plane specification** around your three nodes and then benchmark:  
```
                 YOUR CONTROL PLANE
                         │
             ┌───────────┴───────────┐
             │                       │
          LiteLLM                OmniRoute
             │                       │
       local inference          cloud inference

```
If LiteLLM can do everything you need, simplify to one gateway. If OmniRoute proves substantially better at the cloud-provider side, retain it as a subordinate cloud router.  
The critical design decision is that **Hermes/S.E.E.D. should never need to know which provider or physical node actually performed inference**.  
That abstraction will save you an enormous amount of complexity as the three-node system evolves.  
 ## The network hosts have changed… a fourth host is retired, we now have only the rpi4 and the rpi3. The rpi3 is a DNS sinkhole, honeypot, cybersec bastion securing the network, the rpi4 maintains it’s role and only the Macbook Air M1 16Gb is able to do local inference, for local inference, both Pis route to the Macbook, for cloud inference and simplicity they can also route to the Macbook Air although if the Macbook isn’t available, they can do the cloud inference through their own ollama application as  fallback - the Pis only run cloud models as their hardware is very weak, only the Macbook runs local models. Right now, the only provider is ollama but in the future we may add additional providers ##  Yes — that change actually makes the architecture **much cleaner**. The important distinction is now that you have **one actual local-inference host** and two lightweight infrastructure/agent nodes.  
I would model the current topology as:  
```
                         ┌──────────────────────┐
                         │     MacBook Air M1   │
                         │        16 GB         │
                         │                      │
                         │  LOCAL INFERENCE     │
                         │  Ollama              │
                         │  local models        │
                         │                      │
                         │  CLOUD INFERENCE     │
                         │  Ollama / providers  │
                         └──────────┬───────────┘
                                    │
                         inference gateway
                                    │
                 ┌──────────────────┴──────────────────┐
                 │                                     │
          ┌──────▼───────┐                      ┌──────▼───────┐
          │    RPi 4     │                      │    RPi 3     │
          │              │                      │              │
          │ Hermes/etc.  │                      │ DNS sinkhole │
          │ orchestration│                      │ honeypot     │
          │ services     │                      │ bastion      │
          │              │                      │ cybersec     │
          └──────┬───────┘                      └──────────────┘
                 │
                 │ cloud fallback
                 └──────────────────────► Ollama/provider

```
And there is an important consequence:  
## You probably don't need a complicated distributed inference scheduler yet  
Your current physical topology gives you a very useful simplification:  
**The MacBook is the inference execution plane. The Pis are inference clients.**  
The RPi3 shouldn't really participate in inference scheduling at all. Its primary job is network security/infrastructure.  
The RPi4 can be an inference client/orchestrator.  
So I would initially make:  
```
RPi3 ───────┐
            │
RPi4 ───────┼──► MacBook inference gateway ──► Ollama
            │
MacBook ────┘

```
with:  
```
                         MacBook
                            │
                ┌───────────┴───────────┐
                │                       │
             LOCAL                    CLOUD
                │                       │
         Ollama local             Ollama cloud
                │                       │
        ┌───────┴──────┐          future providers
        │              │
      model A        model B

```
Then the Pis don't need to know whether qwen... is local or cloud.  
They simply ask for inference.  
  
## I would introduce one canonical inference API  
This is the part I'd establish **before** OmniRoute, LiteLLM or anything else.  
For example:  
```
http://macbook:PORT/v1/chat/completions

```
Everything in the lab talks to that endpoint.  
So:  
```
Hermes ────────────────┐
SEED ──────────────────┤
RPi4 applications ────┤
RPi3 security tooling ─┤
future agents ────────┘
                       │
                       ▼
                 INFERENCE API
                       │
                       ▼
                    ROUTER
                       │
              ┌────────┴────────┐
              │                 │
          local Ollama      cloud Ollama

```
This gives you a very valuable invariant:  
**There is exactly one logical inference endpoint for the entire laboratory.**  
The implementation behind it can change.  
  
## Then add fallback at two different levels  
This is where your new topology becomes particularly interesting.  
**Normal path**  
```
RPi4
  │
  ▼
MacBook
  │
  ▼
Ollama
  │
  ├── local model
  │
  └── cloud model

```
**MacBook unavailable**  
```
RPi4
  │
  ├── X MacBook
  │
  ▼
local Ollama on RPi4
  │
  ▼
cloud model

```
Although, because you said the Pis **only run cloud models**, the terminology should perhaps be:  
```
Primary:

RPi → MacBook → Ollama → cloud

Fallback:

RPi → local Ollama → cloud

```
There is no local model inference on the Pi.  
That's a perfectly sensible design.  
  
## This changes my OmniRoute recommendation somewhat  
I would **not put OmniRoute on every machine**.  
I'd instead investigate:  
```
                    ┌────────────────────┐
                    │      MacBook       │
                    │                    │
                    │  Inference Router  │
                    │                    │
                    │ ┌────────────────┐ │
                    │ │     Ollama     │ │
                    │ └────────────────┘ │
                    │                    │
                    │ future providers   │
                    └─────────┬──────────┘
                              │
                    OpenAI-compatible API
                              │
             ┌────────────────┼────────────────┐
             │                                 │
            RPi4                              RPi3

```
**OmniRoute could live on the MacBook** if its provider-routing functionality proves useful.  
Then your Pis remain extremely simple.  
  
## But there is one architectural problem you should solve now  
Suppose Hermes does:  
```
POST /v1/chat/completions
model=qwen3.5

```
What does qwen3.5 mean?  
You don't want:  
```
qwen3.5 = MacBook local model

```
to become an implicit assumption everywhere.  
I'd introduce **logical model classes**.  
For example:  
```
local.reasoning
local.coding
local.fast
cloud.reasoning
cloud.coding
cloud.vision

```
Or, even better, capabilities:  
```
{
  "task": "reasoning",
  "requirements": {
    "context": 32000,
    "structured_output": true,
    "privacy": "local_preferred"
  }
}

```
Then your router resolves that to:  
```
local.reasoning
      ↓
MacBook
      ↓
Ollama
      ↓
qwen3.x

```
That becomes extremely useful when you eventually add:  
```
OpenAI
Anthropic
Gemini
OpenRouter
another Ollama server
etc.

```
because **your agents don't change**.  
  
## I would also separate "routing" from "fallback"  
There are actually two different problems.  
**Routing**  
Where should this request normally go?  
```
reasoning + private
        ↓
MacBook local

```
**Failover**  
What happens when that route doesn't work?  
```
MacBook unavailable
        ↓
RPi4 Ollama
        ↓
cloud

```
These shouldn't be entangled.  
A clean policy could be:  
```
                REQUEST
                   │
                   ▼
             capability match
                   │
                   ▼
            policy evaluation
                   │
                   ▼
             primary route
                   │
              ┌────┴────┐
              │         │
            alive      failed
              │         │
              ▼         ▼
           execute    fallback
                        │
                        ▼
                   next route

```
  
## And I'd make health information explicit  
The MacBook router should expose something like:  
```
{
  "node": "macbook-air-m1",
  "inference": true,
  "ollama": true,
  "local_models": true,
  "cloud_models": true,
  "status": "healthy"
}

```
while the RPi4 might report:  
```
{
  "node": "rpi4",
  "inference": false,
  "cloud_inference_client": true,
  "fallback_provider": "ollama"
}

```
This lets the control plane reason about **capabilities**, rather than guessing from connectivity.  
  
## Your eventual topology could therefore become  
```
                         ┌─────────────────────────┐
                         │       CONTROL PLANE     │
                         │                         │
                         │ policy                  │
                         │ capability registry     │
                         │ routing                 │
                         │ health                  │
                         │ fallback                │
                         │ observability           │
                         └────────────┬────────────┘
                                      │
                              logical inference
                                      │
                         ┌────────────▼────────────┐
                         │       MACBOOK AIR       │
                         │                         │
                         │     LLM GATEWAY         │
                         │          │              │
                         │    ┌─────┴─────┐        │
                         │    │           │        │
                         │ LOCAL       CLOUD       │
                         │ Ollama      Ollama      │
                         │    │           │        │
                         │ local      providers   │
                         │ models      (future)    │
                         └────────────┬────────────┘
                                      │
                         ┌────────────┴────────────┐
                         │                         │
                    ┌────▼────┐               ┌────▼────┐
                    │  RPi4   │               │  RPi3   │
                    │ Hermes  │               │ Security│
                    │ SEED    │               │ DNS     │
                    │ agents  │               │ honeypot│
                    └─────────┘               └─────────┘

```
With **direct Pi → cloud fallback**:  
```
RPi4 ──────X──────► MacBook
  │
  └────────────────► Ollama cloud

RPi3 ──────X──────► MacBook
  │
  └────────────────► Ollama cloud

```
  
## One thing I'd change from my previous recommendation  
Given this topology, I would **not start by deploying LiteLLM + OmniRoute + another router**.  
That would introduce a routing stack before you have demonstrated that you need one.  
I'd benchmark three progressively complex configurations:  
**A — simplest**  
```
RPi → MacBook Ollama

```
with Pi-side fallback.  
**B — provider abstraction**  
```
RPi
 │
 ▼
MacBook
 │
 ▼
LiteLLM
 │
 └── Ollama

```
**C — multi-provider**  
```
RPi
 │
 ▼
Inference Control Plane
 │
 ├── local Ollama
 │
 └── OmniRoute/LiteLLM
       ├── Ollama cloud
       ├── Provider A
       ├── Provider B
       └── Provider C

```
**C is where the architecture becomes genuinely interesting.**  
And I think this is the direction that fits your lab particularly well: **a tiny deterministic inference control plane on the RPi4, with the MacBook acting as the inference gateway/execution host**. That would also fit naturally with the way you've been separating Hermes's deterministic orchestration/control-plane responsibilities from the actual cognitive/inference work.  
If you want, I can next map this into a **concrete RPi4 + RPi3 + MacBook architecture**, including the exact network/API topology, Ollama configuration, health checks, fallback semantics, model registry and a FOSS component comparison (OmniRoute vs LiteLLM vs a small custom router) based on the constraints of your actual machines.  
 ## Map this into a concrete RPi4 + RPi3 + MacBook architecture ##  Absolutely. Given the corrected topology, I would make the **RPi4 the control/orchestration node, the MacBook the inference gateway and only local-model host, and the RPi3 a security/infrastructure node that is also an inference client when necessary**.  
The important design principle is:  
**Inference is a service of the network, not a capability owned by an individual agent.**  
## 1. Concrete topology  
```
                              LAN
                               │
                 ┌─────────────┴─────────────┐
                 │                           │
          ┌──────▼──────┐              ┌─────▼──────┐
          │    RPi 3    │              │   RPi 4    │
          │             │              │            │
          │ DNS         │              │ Hermes     │
          │ Sinkhole    │              │ S.E.E.D.   │
          │ Honeypot    │              │ Agents     │
          │ Bastion     │              │ Control    │
          │ Cybersec    │              │ Plane      │
          │             │              │            │
          │ Ollama      │              │ Ollama     │
          │ CLOUD ONLY  │              │ CLOUD ONLY │
          └──────┬──────┘              └─────┬──────┘
                 │                           │
                 │ inference requests        │
                 │ / fallback                │
                 └─────────────┬─────────────┘
                               │
                               ▼
                    ┌──────────────────────┐
                    │   MacBook Air M1     │
                    │        16 GB         │
                    │                      │
                    │ Inference Gateway    │
                    │        │             │
                    │        ▼             │
                    │      Ollama          │
                    │       /   \           │
                    │      /     \          │
                    │ LOCAL      CLOUD      │
                    │ MODELS     MODELS    │
                    │                      │
                    │ Future providers     │
                    └──────────────────────┘

```
I would **not** make the RPi3 a member of the control plane. It should remain a security appliance.  
  
## 2. Responsibilities  

| Node | Primary role | Inference role |
| ---------- | ------------------------------------------------- | ----------------------------------------- |
| RPi3 | DNS sinkhole, honeypot, bastion, network security | Client; cloud only |
| RPi4 | Hermes, S.E.E.D., orchestration, services | Client; cloud only |
| MacBook M1 | Inference server/gateway | Only local inference host + cloud gateway |
  
This gives you a very clean separation.  
**RPi3**  
It should know only:  
```
"inference endpoint = X"

```
It shouldn't know:  
```
"Qwen is running on the Mac"
"Claude is provider B"
"model X should fail over to Y"

```
That's not its job.  
**RPi4**  
Hermes/S.E.E.D. can request inference, but should likewise request **capabilities**, not physical infrastructure.  
**MacBook**  
This becomes the **Inference Fabric Gateway**.  
  
## 3. The MacBook should expose one API  
I would standardise the entire lab around an OpenAI-compatible endpoint:  
```
http://macbook:PORT/v1/

```
For example:  
```
/v1/chat/completions
/v1/models
/v1/embeddings

```
The exact port doesn't matter yet.  
Then:  
```
Hermes ───────┐
S.E.E.D. ────┤
RPi4 apps ───┤
RPi3 tools ──┤
future nodes ┘
              │
              ▼
       http://macbook/...

```
Everything above this interface becomes independent of Ollama.  
That is important.  
Today:  
```
Gateway → Ollama

```
Tomorrow:  
```
Gateway → Ollama
        → OpenAI
        → Anthropic
        → Gemini
        → OpenRouter
        → another local server

```
Nothing upstream needs to change.  
  
## 4. Don't expose Ollama directly to the Pis  
I'd make:  
```
RPi4 ─────► MacBook Gateway ─────► Ollama
RPi3 ─────► MacBook Gateway ─────► Ollama

```
rather than:  
```
RPi4 ─────► MacBook:11434
RPi3 ─────► MacBook:11434

```
Ollama becomes an **implementation detail**.  
The gateway owns:  
* authentication  
* model aliases  
* routing  
* health  
* concurrency  
* timeouts  
* fallback  
* logging  
* provider abstraction  
This also means you can replace Ollama without redesigning Hermes.  
  
## 5. Model abstraction  
I'd create a small model registry.  
Something conceptually like:  
```
models:

  local-fast:
    capabilities:
      - chat
      - reasoning
    locality: local
    privacy: private
    backend: ollama
    model: <actual-model>

  local-code:
    capabilities:
      - chat
      - coding
      - reasoning
      - structured_output
    locality: local
    privacy: private
    backend: ollama
    model: <actual-model>

  cloud-general:
    capabilities:
      - chat
      - reasoning
    locality: cloud
    privacy: external
    backend: ollama
    model: <cloud-model>

  cloud-vision:
    capabilities:
      - vision
      - reasoning
    locality: cloud
    privacy: external
    backend: ollama
    model: <cloud-model>

```
The actual models can change without changing the agents.  
  
## 6. Use logical model aliases  
For example:  
```
local-fast
local-reasoning
local-code
cloud-fast
cloud-reasoning
cloud-vision

```
Hermes could say:  
```
{
  "model": "local-reasoning"
}

```
The gateway resolves:  
```
local-reasoning
        │
        ▼
MacBook
        │
        ▼
Ollama
        │
        ▼
actual model

```
Later:  
```
cloud-reasoning
        │
        ▼
provider policy
        │
        ├── Ollama Cloud
        ├── Anthropic
        ├── OpenAI
        └── Gemini

```
No agent modifications.  
  
## 7. But eventually capability routing is better  
The next layer is:  
```
{
  "task": "code",
  "requirements": {
    "context": 50000,
    "structured_output": true,
    "tools": true
  },
  "privacy": "local_preferred",
  "priority": 70
}

```
The control plane turns that into:  
```
requirements
     │
     ▼
capability registry
     │
     ├── local-code
     ├── cloud-code
     └── cloud-long-context

```
Then chooses according to policy.  
This is where your system eventually becomes an actual **inference control plane**, rather than a model proxy.  
  
## 8. The fallback chain  
I'd explicitly define the fallback graph.  
For example:  
```
                    REQUEST
                       │
                       ▼
              MacBook available?
                 /           \
               YES            NO
                │              │
                ▼              ▼
          MacBook gateway    RPi-local
                │              │
                ▼              ▼
             Ollama         Ollama
                │              │
                ▼              ▼
             Cloud          Cloud

```
But there is a subtle improvement I'd make:  
**Don't make the RPi fallback blindly equivalent to the MacBook.**  
The registry should say:  
```
rpi-cloud:
  capabilities:
    - chat
    - reasoning
  locality: cloud
  local_inference: false

```
Therefore if someone requests:  
```
privacy = local_only

```
the request simply fails rather than silently leaving the LAN.  
That's a very important security property.  
  
## 9. Privacy policy  
I would make this a first-class routing attribute.  
Three levels are enough initially:  
```
LOCAL_ONLY
LOCAL_PREFERRED
CLOUD_ALLOWED

```
**LOCAL_ONLY**  
```
MacBook local model

```
If MacBook is down:  
```
FAIL

```
**Never send the request to the cloud.**  
**LOCAL_PREFERRED**  
```
MacBook local
      ↓ failure
cloud

```
**CLOUD_ALLOWED**  
```
MacBook/cloud
RPi/cloud
future providers

```
according to availability and policy.  
This gives S.E.E.D. and Hermes an explicit privacy boundary.  
  
## 10. Health model  
The gateway should continuously maintain:  
```
MacBook
 ├── reachable
 ├── gateway healthy
 ├── Ollama healthy
 ├── model available
 ├── cloud reachable
 └── provider health

```
And the RPi4 should maintain:  
```
RPi4
 ├── MacBook reachable?
 ├── inference gateway reachable?
 └── fallback Ollama available?

```
Don't use simply:  
```
ping MacBook

```
as the health check.  
A host can be alive while Ollama is dead.  
Instead:  
```
TCP/API
   ↓
gateway health
   ↓
Ollama health
   ↓
model availability

```
  
## 11. Failure semantics  
I'd define them explicitly.  
**MacBook disappears**  
```
RPi4 → timeout
     → mark gateway unhealthy
     → use cloud fallback

```
**MacBook returns**  
Don't immediately switch every request back.  
Use a recovery condition:  
```
3 consecutive successful health checks
+
model available
+
gateway responsive

```
then:  
```
ACTIVE

```
This avoids flapping.  
  
## 12. The RPi4 becomes the natural control-plane home  
This is where I think your existing architecture gives you an advantage.  
The RPi4 can host:  
```
Hermes
   │
   ├── orchestration
   │
   ├── agent registry
   │
   └── inference policy

```
But I would **not necessarily put the actual inference gateway there**.  
Instead:  
```
RPi4
  │
  │ control
  ▼
Inference Control Plane
  │
  │ requests
  ▼
MacBook Gateway
  │
  ▼
Ollama

```
The distinction is:  
**RPi4 decides.**  
**MacBook executes.**  
That is a very good division for your hardware.  
  
## 13. RPi3 should remain deliberately boring  
I'd resist putting Hermes or a large routing stack on the RPi3.  
Its architecture should remain:  
```
                    INTERNET
                       │
                       ▼
                ┌─────────────┐
                │    RPi3     │
                │             │
                │ DNS         │
                │ sinkhole    │
                │ honeypot    │
                │ bastion     │
                │ monitoring  │
                └──────┬──────┘
                       │
                     LAN

```
If a security process needs LLM assistance:  
```
security tool
     │
     ▼
MacBook gateway

```
and if the MacBook disappears:  
```
security tool
     │
     ▼
RPi3 Ollama
     │
     ▼
cloud

```
But this should be a **degraded emergency path**, not its normal architecture.  
  
## 14. Where OmniRoute fits  
With this architecture:  
```
                   MACBOOK
                      │
              Inference Gateway
                      │
            ┌─────────┴─────────┐
            │                   │
        LOCAL                CLOUD
            │                   │
         Ollama             OmniRoute
                                │
                       ┌────────┼────────┐
                       │        │        │
                    Ollama   Provider B Provider C

```
This is where I now think OmniRoute makes the most sense.  
**It becomes a cloud-provider adapter/router, not your entire inference architecture.**  
You can replace it later with LiteLLM or your own provider router without changing the rest.  
  
## 15. One thing I would *not* do  
Don't create this:  
```
Hermes
  ↓
LiteLLM
  ↓
OmniRoute
  ↓
Ollama

```
at the beginning.  
That's three layers doing potentially overlapping routing.  
Instead:  
```
                    CONTROL
                       │
                       ▼
                Inference Gateway
                       │
                 ┌─────┴─────┐
                 │           │
               Ollama     OmniRoute

```
One decision point.  
One source of truth.  
  
## 16. Recommended initial implementation  
I'd start extremely small:  
```
                     RPi4
                      │
                  Hermes/SEED
                      │
                      │ OpenAI API
                      ▼
               ┌──────────────┐
               │   MacBook    │
               │              │
               │  Gateway     │
               │     │        │
               │   Ollama     │
               └──────────────┘
                      │
                      │ failure
                      ▼
                  RPi4 Ollama
                      │
                      ▼
                   Cloud

```
RPi3:  
```
RPi3 security services
          │
          ▼
     MacBook Gateway
          │
          │ failure
          ▼
     RPi3 Ollama
          │
          ▼
        Cloud

```
Then add provider abstraction **only when you actually introduce provider #2**.  
  
## 17. The interface I'd freeze now  
This is probably the most important part.  
Don't freeze:  
```
Ollama API

```
Freeze:  
```
Inference Service API

```
Conceptually:  
```
POST /v1/chat/completions
POST /v1/embeddings
GET  /v1/models
GET  /health
GET  /health/inference
GET  /health/providers

```
And internally:  
```
Request
   │
   ├── identity
   ├── task
   ├── capabilities
   ├── privacy
   ├── priority
   ├── timeout
   └── model preference
          │
          ▼
       Router
          │
          ▼
      Execution

```
That gives you a stable substrate for Hermes, S.E.E.D. and future agents.  
  
## My recommended final architecture  
```
                         ┌──────────────────────────┐
                         │        RPi 4             │
                         │                          │
                         │ Hermes                   │
                         │ S.E.E.D.                 │
                         │ Agent orchestration      │
                         │ Inference policy         │
                         │                          │
                         │ Fallback Ollama ─────┐   │
                         └──────────┬───────────┼───┘
                                    │           │
                                    │           │ CLOUD
                                    │           ▼
                                    │       Ollama Cloud
                                    │
                                    │ PRIMARY
                                    ▼
                         ┌──────────────────────────┐
                         │      MacBook Air M1      │
                         │          16 GB           │
                         │                          │
                         │  INFERENCE GATEWAY      │
                         │          │               │
                         │     ┌────┴────┐          │
                         │     │         │          │
                         │  Ollama    Cloud Router │
                         │     │         │          │
                         │ LOCAL       future      │
                         │ MODELS      providers    │
                         └──────────┬───────────────┘
                                    │
                                    │
                         ┌──────────▼───────────────┐
                         │         RPi 3            │
                         │                          │
                         │ DNS sinkhole             │
                         │ Honeypot                  │
                         │ Cybersec bastion         │
                         │                          │
                         │ Fallback Ollama ────────►│
                         │                          │
                         └──────────────────────────┘

```
**The three rules I'd freeze into the architecture**  
**1. MacBook = only local inference host.**  
**2. RPi4 = cognitive/orchestration/control-plane host, not inference hardware.**  
**3. RPi3 = security appliance, not part of the cognitive control plane.**  
Then put a **single logical Inference Service API** between all agents and the inference infrastructure.  
That gives you a surprisingly clean system today while leaving a natural path to multiple providers tomorrow. It also avoids prematurely turning a three-machine homelab into a distributed Kubernetes-like inference platform when the actual hardware topology doesn't require that complexity yet.  
 ## Hermes is only running on the rpi4. i’d like to use DSH or Claude that run on the Mac to develop and build this. I want everything to be lightweight and without unnecessary bloat. If I understand correctly the first step is to develop and build the control plane on the roi4 and then the inference router on the Mac? ##  Yes — with one refinement: **build the control-plane contract first, but don't build two independent routing systems.** The RPi4 control plane should own *policy and orchestration*; the Mac should own *inference execution and provider routing*. Keep both extremely small.  
Your topology becomes:  
```
                    ┌─────────────────────────┐
                    │        RPi4             │
                    │                         │
                    │ Hermes                  │
                    │ Control Plane           │
                    │                         │
                    │ • task/request state    │
                    │ • routing policy        │
                    │ • capabilities          │
                    │ • health                │
                    │ • fallback policy       │
                    │ • telemetry             │
                    └────────────┬────────────┘
                                 │
                         inference request
                                 │
                                 ▼
                    ┌─────────────────────────┐
                    │       MacBook M1        │
                    │                         │
                    │ Inference Gateway       │
                    │                         │
                    │ • model registry        │
                    │ • provider adapters     │
                    │ • local/cloud routing  │
                    │ • concurrency           │
                    │ • retries/fallback      │
                    └────────────┬────────────┘
                                 │
                    ┌────────────┴────────────┐
                    │                         │
                    ▼                         ▼
              Ollama local              Ollama cloud
              M1 models                 / future providers

```
And the **RPi3 stays out of the control plane**:  
```
RPi3
 ├── DNS sinkhole
 ├── honeypot
 ├── bastion
 └── security services
          │
          └── inference client → Mac
                         │
                         └── fallback → cloud

```
## But I would develop it in this order  
**Phase 0 — freeze the contract**  
Before writing either router, define a tiny inference.v1 protocol.  
Something like:  
```
{
  "request_id": "uuid",
  "source": "hermes",
  "task": "reasoning",
  "model": "local-reasoning",
  "messages": [],
  "requirements": {
    "context": 32000,
    "tools": false,
    "structured_output": false
  },
  "policy": {
    "privacy": "local_preferred",
    "priority": 50,
    "timeout_ms": 60000
  }
}

```
The response should contain enough information to make the system observable:  
```
{
  "request_id": "uuid",
  "status": "success",
  "model": "qwen...",
  "execution_host": "macbook",
  "provider": "ollama",
  "locality": "local",
  "latency_ms": 1234,
  "usage": {
    "input_tokens": 1000,
    "output_tokens": 500
  }
}

```
**This protocol is more important than choosing LiteLLM vs OmniRoute.**  
  
## Phase 1 — build the tiny RPi4 control plane  
Since Hermes already exists there, I'd make this another small Hermes subsystem rather than introducing another heavyweight daemon.  
The RPi4 control plane answers:  
**What should happen?**  
It should know:  
```
request
   ↓
requirements
   ↓
policy
   ↓
candidate execution class
   ↓
primary + fallback route

```
For example:  
```
LOCAL_ONLY
    ↓
MacBook/local

LOCAL_PREFERRED
    ↓
MacBook/local
    ↓ failure
MacBook/cloud

CLOUD_ALLOWED
    ↓
MacBook/cloud
    ↓ failure
RPi4/cloud

```
It should **not** know how Ollama works internally.  
  
## Phase 2 — build the Mac inference gateway  
Then Claude/DSH can build the Mac component.  
Its job is:  
**How do I actually execute this inference request?**  
Initially this can be almost ridiculously small:  
```
HTTP API
   ↓
model registry
   ↓
Ollama

```
For example:  
```
/v1/chat/completions
        │
        ▼
     router
        │
   ┌────┴────┐
   │         │
 local      cloud
   │         │
 Ollama     Ollama

```
Don't add OmniRoute yet.  
Don't add LiteLLM yet.  
Don't add Redis.  
Don't add Kubernetes.  
Don't add a message broker.  
Don't add a database unless the implementation proves one is necessary.  
For your current topology, **SQLite or even an append-only event log may be sufficient for persistent state**, depending on what you actually need.  
  
## Phase 3 — prove the entire path  
Get this working first:  
```
Hermes
  │
  │ inference.v1
  ▼
RPi4 Control Plane
  │
  │ route = macbook/local
  ▼
Mac Gateway
  │
  ▼
Ollama
  │
  ▼
local model

```
Then test:  
```
MacBook unavailable
        │
        ▼
RPi4 detects failure
        │
        ▼
RPi4 Ollama
        │
        ▼
cloud model

```
Then:  
```
MacBook returns
        │
        ▼
health restored
        │
        ▼
primary route restored

```
At that point you have a functioning system.  
  
## Phase 4 — only then add provider abstraction  
When you actually introduce provider #2:  
```
                  Mac Gateway
                       │
             ┌─────────┴─────────┐
             │                   │
           Ollama             Provider
             │                   │
        ┌────┴────┐          ┌───┴───┐
        │         │          │       │
      local     cloud        A       B

```
**That** is when OmniRoute/LiteLLM becomes worth evaluating.  
You can put it behind your existing interface:  
```
RPi4
 │
 ▼
Control Plane
 │
 ▼
Mac Inference Gateway
 │
 ├── Ollama
 │
 └── provider-router
       ├── provider A
       ├── provider B
       └── provider C

```
The rest of the system doesn't care which one you chose.  
  
## One correction to the terminology  
I'd avoid thinking of these as:  
**control plane + inference router**  
because it can encourage two components to make overlapping routing decisions.  
I'd define them as:  
**RPi4 — Inference Control Plane**  
Owns:  
* identity  
* policy  
* capability requirements  
* priority  
* privacy  
* primary/fallback selection  
* health state  
* request lifecycle  
**Mac — Inference Gateway**  
Owns:  
* actual model execution  
* Ollama  
* local/cloud distinction  
* provider adapters  
* concurrency  
* provider-specific retries  
* provider health  
* model discovery  
So:  
```
             WHAT SHOULD HAPPEN?
                    │
                    ▼
              RPi4 Control
                    │
                    │
             WHERE/HOW?
                    │
                    ▼
             Mac Gateway
                    │
                    ▼
             WHAT EXECUTES?
                    │
                    ▼
                  Ollama

```
That separation is clean.  
  
## And I'd use DSH/Claude exactly as you suggest  
I wouldn't ask an agent to "build an LLM router".  
I'd give it a tightly bounded engineering mission:  
**Implement the smallest possible inference control-plane service according to this protocol. Do not introduce infrastructure, dependencies or abstractions unless the specification requires them.**  
Then separately:  
**Implement the smallest possible inference gateway for macOS/Apple Silicon. Ollama is the sole provider initially. Preserve the provider interface so additional providers can be added later, but do not implement them yet.**  
This is particularly compatible with the way you've been developing Hermes: **specification first, deterministic behaviour second, implementation third, complexity only when demonstrated necessary.**  
**So the immediate build sequence I'd use is:**  
```
1. inference.v1 specification
             ↓
2. RPi4 control-plane implementation
             ↓
3. Mac inference-gateway implementation
             ↓
4. End-to-end tests
             ↓
5. Failure/fallback tests
             ↓
6. Load/concurrency tests
             ↓
7. Only then evaluate OmniRoute/LiteLLM
             ↓
8. Add provider #2 when actually needed

```
That is the route I'd take for your lab. **Small, deterministic, observable, and provider-agnostic without prematurely building a distributed platform.**  
 ## Very well, this seems like a sensible strategy. Let’s proceed with it…  
  
Suggest a name for this project, keep the project in your memory while we develop it so you can help me not drifting.  
  
Generate and build a robust and reliable roadmap plan  for the project and all it’s phases, it can be adjusted to our discoveries and requirements but should set the course to prevent drifting and ensure a consistent and reliable development of the project.  
  
You cab explain to me what is happening but the execution instructions should come in single block prompts that I can paste into Claude or the DSH to execute - preferably long running comprehensive prompts that allow Claude or DSH to have all the information needed to autonomously develop and build the project without my constant intervention ##  Yes. I think we now have enough architectural clarity to **freeze a course without freezing the implementation**.  
I would name the project **Lattice** — more formally **Inference Lattice**.  
The name fits because the system is not merely a router: it is a small network of inference capabilities, policies and execution paths connected through a stable interface. It also leaves room for future providers and nodes without baking today's topology into the name.  
## Inference Lattice  
**Core principle**  
**Lattice separates the decision of what inference should happen from the mechanism that performs it.**  
```
                         INFERENCE LATTICE

              ┌──────────────────────────────┐
              │          RPi4                │
              │                              │
              │       LATTICE CONTROL        │
              │                              │
              │  policy · capability ·      │
              │  lifecycle · health ·       │
              │  fallback · observability   │
              └──────────────┬───────────────┘
                             │
                       inference.v1
                             │
                             ▼
              ┌──────────────────────────────┐
              │        MacBook M1            │
              │                              │
              │       LATTICE GATEWAY        │
              │                              │
              │  execution · Ollama ·        │
              │  provider adapters ·        │
              │  concurrency · health       │
              └──────────────┬───────────────┘
                             │
                    ┌────────┴────────┐
                    │                 │
                    ▼                 ▼
                 LOCAL              CLOUD
                 Ollama             Ollama

```
The **RPi3 is deliberately outside Lattice Control**. It is an inference client/security appliance.  
  
## The roadmap  
I would establish the following as the project's **development constitution**.  
It is deliberately staged. Each phase has a concrete exit condition, and later complexity is not allowed to leak backwards into earlier phases.  
## Phase 0 — Charter, repository and architectural baseline  
**Purpose:** establish the project before writing implementation code.  
Freeze:  
* topology  
* responsibilities  
* terminology  
* architectural boundaries  
* non-goals  
* repository structure  
* development rules  
* test philosophy  
* decision record mechanism  
**Exit condition**  
A fresh developer/agent can understand the architecture without talking to us.  
  
## Phase 1 — inference.v1 protocol  
This is the most important phase.  
Define:  
* request envelope  
* response envelope  
* errors  
* request identity  
* agent identity  
* capabilities  
* model aliases  
* privacy policy  
* priority  
* timeout  
* cancellation  
* health  
* execution metadata  
* versioning  
Example:  
```
Agent
  │
  │ inference.v1
  ▼
Lattice Control
  │
  │ execution request
  ▼
Lattice Gateway
  │
  ▼
Provider

```
**Exit condition**  
We can describe a complete request/response lifecycle without mentioning Ollama.  
  
## Phase 2 — RPi4 Lattice Control  
Build the smallest deterministic control-plane service.  
Responsibilities:  
```
request
   ↓
validate
   ↓
policy
   ↓
capability selection
   ↓
primary route
   ↓
fallback route
   ↓
execution request
   ↓
result

```
Initially:  
```
primary = MacBook
fallback = RPi4 cloud Ollama

```
No multi-provider abstraction beyond what the protocol requires.  
**Exit condition**  
Hermes can request inference through Lattice without knowing how or where the model runs.  
  
## Phase 3 — MacBook Lattice Gateway  
Build the Mac component.  
Initially:  
```
Lattice Gateway
      │
      ▼
    Ollama

```
Nothing else.  
It should provide:  
* OpenAI-compatible interface where useful  
* Lattice internal interface  
* model registry  
* model aliases  
* health  
* execution  
* timeout handling  
* concurrency limits  
* structured telemetry  
**Exit condition**  
A request originating on RPi4 reaches a Mac local model and returns a validated result.  
  
## Phase 4 — End-to-end integration  
Now connect:  
```
Hermes
 ↓
RPi4 Control
 ↓
Mac Gateway
 ↓
Ollama
 ↓
local model

```
Test:  
* normal requests  
* concurrent requests  
* malformed requests  
* unavailable models  
* timeout  
* cancellation  
* oversized context  
* structured output  
* tool requests if applicable  
**Exit condition**  
The entire path behaves deterministically under normal and abnormal conditions.  
  
## Phase 5 — Failure and fallback  
This is where Lattice becomes infrastructure rather than merely an API wrapper.  
Test:  
```
MacBook unavailable
       ↓
RPi4 detects failure
       ↓
RPi4 local Ollama
       ↓
cloud model

```
Then:  
```
MacBook returns
       ↓
health recovery
       ↓
primary route restored

```
Also test:  
* Ollama unavailable  
* cloud unavailable  
* network partition  
* stale health  
* repeated failures  
* recovery/flapping  
* simultaneous failures  
* duplicate requests  
**Exit condition**  
We can deliberately break each dependency and predict the resulting behaviour.  
  
## Phase 6 — RPi3 integration  
Only after the core system is reliable.  
RPi3 becomes:  
```
security service
      │
      ▼
Lattice client
      │
      ▼
Mac Gateway

```
Fallback:  
```
Mac unavailable
      ↓
RPi3 Ollama
      ↓
cloud

```
Importantly:  
**RPi3 does not become a control-plane node.**  
**Exit condition**  
Security services can consume inference without compromising the RPi3's primary security role.  
  
## Phase 7 — Observability and operational hardening  
Add only what actual testing demonstrates is useful.  
Potentially:  
* structured logs  
* request IDs  
* correlation IDs  
* latency  
* token usage  
* routing decisions  
* failure reasons  
* provider health  
* queue depth  
* concurrency  
* circuit breakers  
* resource limits  
* metrics endpoint  
A request should become traceable:  
```
req-7f21
  │
  ├── Hermes
  ├── RPi4
  ├── route: macbook/local
  ├── gateway
  ├── Ollama
  ├── model
  ├── latency
  └── result

```
**Exit condition**  
A failed inference can be diagnosed from logs without guessing.  
  
## Phase 8 — Provider abstraction  
**Only now** should we seriously evaluate OmniRoute, LiteLLM or another provider router.  
When provider #2 arrives:  
```
                    Lattice
                       │
                 Mac Gateway
                       │
             ┌─────────┴─────────┐
             │                   │
           Ollama             Provider
                               Router
                            ┌────┼────┐
                            │    │    │
                            A    B    C

```
Then benchmark:  
* OmniRoute  
* LiteLLM  
* direct adapters  
* possibly a tiny native implementation  
The decision is empirical.  
**Exit condition**  
A second provider can be introduced without changing Hermes or the RPi4 control protocol.  
  
## Phase 9 — Advanced scheduling  
Only if actual workload demands it.  
Potential future capabilities:  
* weighted routing  
* cost optimisation  
* latency-aware routing  
* queue-aware routing  
* model scoring  
* capability negotiation  
* workload classes  
* quotas  
* per-agent budgets  
* priority queues  
* streaming  
* speculative execution  
* parallel inference  
* multi-model workflows  
These are **explicitly future work**.  
We do not build them because they sound interesting.  
  
## Phase 10 — Lattice 2.x  
Only after real-world operation should we consider whether Lattice has become a general distributed inference fabric.  
Potentially:  
```
                Lattice Control
                      │
       ┌──────────────┼──────────────┐
       │              │              │
    Mac M1         future node    cloud
       │
  local/cloud

```
At that point we can reconsider whether the architecture should remain lightweight or evolve into something larger.  
  
## The anti-drift rules  
I recommend we make these almost sacred.  
**1. Ollama is an implementation detail**  
Never allow Ollama concepts to leak unnecessarily into inference.v1.  
**2. The RPi4 decides; the Mac executes**  
The control plane determines **policy and route**.  
The gateway determines **execution mechanics**.  
**3. RPi3 remains a security appliance**  
No unnecessary cognitive/control-plane responsibilities.  
**4. One canonical protocol**  
Agents should not develop bespoke inference APIs.  
**5. No speculative infrastructure**  
No:  
* Kubernetes  
* Redis  
* Kafka  
* RabbitMQ  
* databases for trivial state  
* service mesh  
* distributed consensus  
* containers everywhere  
* heavyweight observability stacks  
unless testing proves they are necessary.  
**6. No premature provider abstraction**  
Provider #1 is Ollama.  
Provider #2 justifies abstraction.  
**7. Deterministic infrastructure**  
LLMs may **request** inference.  
LLMs should not be trusted to arbitrarily modify routing policy or infrastructure state.  
**8. Every phase has an exit test**  
We don't progress because an implementation "looks finished".  
We progress because a defined invariant has been demonstrated.  
**9. Preserve reversibility**  
Every significant architectural decision should be recorded, with the reason and alternatives considered.  
**10. Complexity must earn its existence**  
This should probably become Lattice's unofficial engineering motto:  
**If removing a component does not break a demonstrated requirement, remove the component.**  
  
## How I suggest we work with Claude / DSH  
I agree with your preference for **long-running autonomous prompts**.  
Rather than repeatedly telling the agent:  
"Now build this."  
we give it the architecture, constraints, current phase, exit criteria and authority to inspect the system, implement, test, document and report.  
The agent should be able to spend an hour working without needing us to answer trivial questions.  
But there is one important safeguard:  
**Autonomy within a phase; architectural authority remains bounded by the roadmap.**  
In other words, Claude can decide *how* to implement Phase 2.  
It cannot decide:  
"I've decided Lattice needs Kubernetes."  
without demonstrating a requirement that forces that conclusion.  
  
## First execution prompt  
I recommend we start with **Phase 0 only**.  
Do not have Claude immediately write the control plane. We want it to inspect the RPi4, establish the repository, verify assumptions, and produce the actual implementation specification before coding.  
Here is the first autonomous prompt I would give it:  
## LATTICE — PHASE 0 BOOTSTRAP  
You are the primary autonomous development agent for a project called **Lattice**, a lightweight distributed inference control and execution system for a small personal homelab.  
Your job is to inspect the existing environment, establish the project correctly, document the architecture, identify contradictions or missing information, and prepare the project for implementation.  
Do not rush into implementation.  
This is Phase 0. The objective is to establish a reliable foundation and prevent architectural drift.  
  
## 1. PROJECT IDENTITY  
Project name:  
**Lattice**  
Formal description:  
**Inference Lattice — a lightweight control and execution fabric that separates inference policy and orchestration from model/provider execution.**  
Core architectural principle:  
Lattice separates the decision of WHAT inference should happen and WHERE it should be executed from HOW the inference provider actually performs it.  
The system must remain lightweight, deterministic, observable, provider-agnostic at its public protocol boundary, and free of unnecessary infrastructure.  
  
## 2. CURRENT HARDWARE TOPOLOGY  
There are exactly three relevant hosts.  
**RPi4**  
Role:  
* Hermes host  
* S.E.E.D. host  
* orchestration/agent host  
* future Lattice Control host  
* cloud-inference client  
* fallback inference host  
The RPi4 does NOT perform useful local LLM inference.  
It may run an Ollama installation capable of accessing cloud models, but it is not a local-model inference node.  
The RPi4 is the intended home of:  
**Lattice Control**  
The RPi4 should determine:  
* request validity  
* policy  
* capabilities  
* privacy constraints  
* priority  
* route selection  
* fallback policy  
* request lifecycle  
* health state  
It should NOT implement provider-specific inference mechanics.  
  
**MacBook Air M1, 16 GB**  
This is the ONLY host in the current network capable of useful local model inference.  
Role:  
* local inference host  
* inference gateway  
* Ollama host  
* cloud inference gateway  
* future provider integration point  
The MacBook is the intended home of:  
**Lattice Gateway**  
The MacBook should perform:  
* inference execution  
* Ollama integration  
* local model execution  
* cloud model execution through Ollama  
* provider adapters in the future  
* provider-specific execution details  
* concurrency management  
* execution health  
* execution telemetry  
Initially the ONLY inference provider is:  
**Ollama**  
Do not add additional providers during Phase 0.  
  
**RPi3**  
Role:  
* DNS sinkhole  
* honeypot  
* cybersecurity bastion  
* network security infrastructure  
The RPi3 is NOT part of the Lattice control plane.  
It is an inference CLIENT only.  
It may request inference through the MacBook.  
If the MacBook is unavailable, it may use its own Ollama installation for cloud inference as a fallback.  
Do not turn the RPi3 into an inference scheduler, orchestration node or general-purpose Lattice control node.  
Its security role takes precedence over inference.  
  
## 3. TARGET ARCHITECTURE  
The intended architecture is:  
```
RPi4
  │
  │ Hermes / S.E.E.D.
  │
  ▼
Lattice Control
  │
  │ inference.v1
  ▼
MacBook
  │
  ▼
Lattice Gateway
  │
  ▼
Ollama
  │
  ├── local models
  │
  └── cloud models

```
RPi3:  
```
RPi3 security services
  │
  ▼
Lattice client
  │
  ▼
MacBook Gateway
  │
  └── fallback → RPi3 Ollama → cloud

```
RPi4 fallback:  
```
RPi4
  │
  ├── primary → MacBook Gateway
  │
  └── fallback → RPi4 Ollama → cloud

```
The architecture must not require the MacBook to be available for the RPi4/RPi3 to perform cloud inference.  
However, local inference is available ONLY through the MacBook.  
  
## 4. IMPORTANT ARCHITECTURAL SEPARATION  
Do not collapse the following concepts:  
**Lattice Control**  
Answers:  
What should happen?  
Responsibilities:  
* policy  
* identity  
* capability requirements  
* privacy  
* priority  
* routing decision  
* fallback decision  
* lifecycle  
* health state  
**Lattice Gateway**  
Answers:  
How is this request actually executed?  
Responsibilities:  
* provider execution  
* Ollama  
* model discovery  
* model mapping  
* provider health  
* concurrency  
* timeouts  
* provider-specific failures  
* execution telemetry  
**Ollama**  
Answers:  
How do I actually run/access this model?  
Ollama is an implementation detail.  
Do not let Ollama's internal concepts become unnecessarily embedded into the Lattice protocol.  
  
## 5. PUBLIC ARCHITECTURAL BOUNDARY  
The system will eventually expose a stable inference protocol called:  
**inference.v1**  
The protocol should not depend on Ollama.  
At minimum, investigate the need for:  
* request ID  
* source/agent identity  
* task  
* model preference or logical model alias  
* messages/input  
* capabilities  
* context requirements  
* tool requirements  
* structured-output requirements  
* privacy policy  
* priority  
* timeout  
* cancellation  
* response status  
* selected execution host  
* selected provider  
* selected model  
* locality  
* timing  
* usage information  
* error classification  
Do NOT implement the complete protocol during Phase 0 unless necessary for specification validation.  
Instead, establish the requirements and draft the protocol specification.  
  
## 6. INITIAL PRIVACY MODEL  
The system should eventually support at least:  
LOCAL_ONLY LOCAL_PREFERRED CLOUD_ALLOWED  
Semantics:  
LOCAL_ONLY:  
* only MacBook local inference is acceptable  
* if unavailable, fail  
* NEVER silently send the request to cloud  
LOCAL_PREFERRED:  
* prefer MacBook local inference  
* cloud fallback is permitted  
CLOUD_ALLOWED:  
* cloud inference may be used according to routing policy  
Do not implement elaborate privacy machinery during Phase 0.  
Document the semantics.  
  
## 7. INITIAL ROUTING MODEL  
The intended initial routing policy is:  
Normal:  
```
RPi4/RPi3
   ↓
MacBook Gateway
   ↓
Ollama
   ↓
local or cloud model

```
MacBook unavailable:  
```
RPi4
   ↓
RPi4 Ollama
   ↓
cloud model

```
or:  
```
RPi3
   ↓
RPi3 Ollama
   ↓
cloud model

```
The exact health/recovery mechanism will be designed in later phases.  
Do not invent a complex distributed scheduler.  
  
## 8. CURRENT PROVIDER  
There is exactly one provider:  
**Ollama**  
Do not install or integrate:  
* LiteLLM  
* OmniRoute  
* OpenRouter  
* OpenAI  
* Anthropic  
* Gemini  
* additional provider gateways  
during Phase 0.  
These may be evaluated later when there is an actual second provider requirement.  
  
## 9. DESIGN PHILOSOPHY  
The project must follow these rules:  
1. Minimal dependencies.  
2. Prefer standard library facilities where practical.  
3. Avoid unnecessary daemons.  
4. Avoid unnecessary databases.  
5. Avoid message brokers unless demonstrated necessary.  
6. Avoid Kubernetes.  
7. Avoid service meshes.  
8. Avoid distributed consensus.  
9. Avoid unnecessary containers.  
10. Avoid premature abstractions.  
11. Prefer deterministic behaviour.  
12. Prefer explicit state over implicit behaviour.  
13. Prefer testability over cleverness.  
14. Prefer simple failure modes.  
15. Every dependency must have a demonstrated reason to exist.  
16. Every architectural component must have a clearly defined responsibility.  
17. Do not duplicate routing logic between Control and Gateway.  
18. Do not allow agents to depend directly on physical hosts or provider names unnecessarily.  
19. Preserve reversibility.  
20. Record significant architectural decisions.  
Engineering principle:  
If removing a component does not break a demonstrated requirement, remove the component.  
  
## 10. PHASE 0 OBJECTIVES  
Perform the following work autonomously.  
**A. Inspect the RPi4**  
Determine:  
* OS  
* architecture  
* available compiler/toolchains  
* available runtimes  
* existing Hermes structure  
* existing S.E.E.D. structure  
* existing Ollama configuration  
* network configuration  
* service management  
* available resources  
* existing development conventions  
* existing repositories relevant to this project  
Do not modify unrelated Hermes/S.E.E.D. infrastructure.  
Do not modify production services without explicit necessity.  
  
**B. Determine an appropriate project location**  
Inspect existing development directories.  
Choose a sensible location for Lattice based on existing conventions.  
Do not create duplicate project roots.  
Document why the location was chosen.  
  
**C. Establish repository structure**  
Create the minimum useful repository structure.  
The exact structure is your implementation decision, but it should support at least:  
* architecture documentation  
* protocol specification  
* ADRs/decision records  
* tests  
* future control-plane implementation  
* future gateway implementation  
* operational documentation  
Do not create empty speculative subsystems.  
  
**D. Create the project charter**  
Create a canonical project document containing:  
* purpose  
* scope  
* topology  
* responsibilities  
* architectural boundaries  
* current provider  
* privacy model  
* fallback model  
* non-goals  
* development philosophy  
* roadmap  
* acceptance criteria  
The charter must clearly distinguish:  
CURRENT  
from:  
FUTURE  
and:  
NOT PLANNED.  
  
**E. Create architecture documentation**  
Document the current intended architecture.  
Include at minimum:  
1. logical architecture  
2. physical topology  
3. request lifecycle  
4. control/gateway separation  
5. failure model  
6. privacy model  
7. future provider model  
8. RPi3 boundary  
9. RPi4 responsibilities  
10. MacBook responsibilities  
Use diagrams where useful, preferably Mermaid if appropriate.  
  
**F. Create an ADR system**  
Establish lightweight architectural decision records.  
The first ADRs should address:  
* why Lattice exists  
* why RPi4 owns control  
* why MacBook owns inference execution  
* why RPi3 is outside the control plane  
* why Ollama is initially the sole provider  
* why provider routers are deferred  
* why inference.v1 is provider-neutral  
* why unnecessary infrastructure is prohibited  
Do not over-document trivial implementation choices.  
  
**G. Draft inference.v1**  
Create an initial protocol specification.  
It does NOT need to be final.  
Mark unresolved questions explicitly.  
Distinguish:  
REQUIRED  
OPTIONAL  
FUTURE  
UNKNOWN  
Do not silently invent requirements.  
  
**H. Establish the roadmap**  
Create a canonical roadmap with these phases:  
1. Charter and baseline  
2. inference.v1 protocol  
3. RPi4 Lattice Control  
4. MacBook Lattice Gateway  
5. End-to-end integration  
6. Failure and fallback  
7. RPi3 integration  
8. Observability/hardening  
9. Provider abstraction  
10. Advanced scheduling  
11. Lattice 2.x evaluation  
Each phase must have:  
* objective  
* scope  
* non-goals  
* implementation outputs  
* tests  
* exit criteria  
  
## 11. DRIFT PREVENTION  
Create a file that functions as a project guardrail.  
It should explicitly state:  
* current topology  
* current phase  
* current provider  
* responsibilities of each node  
* architectural invariants  
* prohibited premature complexity  
* next phase  
* exit criteria for current phase  
An autonomous agent working on this repository must read this file before making architectural changes.  
If a requested change conflicts with the guardrails:  
1. identify the conflict  
2. document it  
3. determine whether the requirement genuinely invalidates the current architecture  
4. do not silently alter the architecture  
  
## 12. DO NOT IMPLEMENT PHASE 2 OR 3 YET  
This is critical.  
Do NOT start building the control plane.  
Do NOT start building the gateway.  
Do NOT install LiteLLM.  
Do NOT install OmniRoute.  
Do NOT implement multi-provider routing.  
Do NOT introduce a database merely because future state may need one.  
Do NOT build a distributed scheduler.  
Do NOT redesign Hermes.  
Do NOT redesign S.E.E.D.  
Phase 0 is successful only when the project has a clean, inspectable foundation for implementation.  
  
## 13. TESTING EXPECTATIONS  
Even though implementation is not yet being built, establish the future testing philosophy.  
Tests should eventually cover:  
* protocol validation  
* routing decisions  
* privacy constraints  
* capability matching  
* health state  
* failure  
* timeout  
* fallback  
* recovery  
* concurrency  
* malformed provider responses  
* unavailable models  
* network failure  
The test architecture should permit deterministic simulation of failures without requiring the physical network for every test.  
  
## 14. DOCUMENTATION STYLE  
Use concise technical documentation.  
Avoid marketing language.  
Avoid speculative claims.  
Avoid describing Lattice as an "AI brain", "autonomous organism", "mesh intelligence" or similar language.  
This is infrastructure.  
Treat it as engineering infrastructure.  
  
## 15. AUTONOMY  
You have authority to:  
* inspect the system  
* create the repository  
* create documentation  
* create ADRs  
* create specifications  
* create test plans  
* make low-risk implementation-structure decisions  
You do NOT have authority to:  
* redesign Hermes  
* redesign S.E.E.D.  
* modify unrelated services  
* install unnecessary infrastructure  
* expose services to the Internet  
* weaken firewall/security configuration  
* change DNS/security infrastructure  
* introduce additional LLM providers  
* change the fundamental Lattice topology  
If you discover that an assumption is false, document the discovery and adapt the design while preserving the project's core principles.  
  
## 16. COMPLETION CRITERIA  
Do not report Phase 0 complete merely because files were created.  
Phase 0 is complete only when:  
1. The repository exists in the correct location.  
2. The repository has a clean initial structure.  
3. The architecture is documented.  
4. The three-host topology is documented correctly.  
5. RPi4/RPi3/Mac responsibilities are unambiguous.  
6. The control/gateway boundary is documented.  
7. The inference.v1 draft exists.  
8. Privacy semantics are documented.  
9. Failure/fallback semantics are documented.  
10. The roadmap exists.  
11. The drift-prevention document exists.  
12. ADRs record the foundational decisions.  
13. No unnecessary runtime dependencies were introduced.  
14. No unrelated production infrastructure was changed.  
15. Repository state is clean and reproducible.  
16. A concise Phase 0 completion report exists.  
  
## 17. FINAL REPORT  
When finished, report:  
**Environment discovered**  
* OS  
* architecture  
* relevant runtimes  
* existing services  
* relevant existing repositories  
**Repository**  
* exact location  
* structure  
* initial commit if appropriate  
**Architecture**  
* confirmed topology  
* any deviations discovered  
**Specifications**  
* documents created  
* unresolved questions  
**Decisions**  
* ADRs created  
* important assumptions  
**Security**  
* ports/services changed  
* network exposure  
* confirmation that unrelated security infrastructure was not altered  
**Testing**  
* validation performed  
**Phase status**  
State exactly:  
PHASE 0 COMPLETE  
or:  
PHASE 0 BLOCKED  
If blocked, identify the precise blocker and do not conceal it.  
**Next phase**  
The next intended phase is:  
**Phase 1 — inference.v1 protocol**  
Do not begin Phase 1 implementation unless explicitly instructed.  
  
Operate autonomously, inspect before modifying, keep the implementation minimal, document decisions, test what you change, and preserve the architectural boundaries above.  
That is the prompt I would use **now**, before giving Claude/DSH permission to start building.  
Once Phase 0 comes back clean, the next prompt should be a similarly self-contained **Phase 1 protocol-engineering prompt**. After that we can give it the Phase 2 control-plane prompt, then Phase 3 gateway prompt, etc. This keeps the agent autonomous while ensuring that it cannot quietly turn a small three-machine inference fabric into an unnecessarily elaborate platform.  
And I will keep **Inference Lattice / Lattice**, its topology, phase roadmap, and architectural boundaries as our continuity point while we develop it.  
