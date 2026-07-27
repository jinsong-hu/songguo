# Proxy Implementation Takeaway

This repository does not implement a Git transport proxy. It implements a
transparent AI API gateway. This note is the shortest implementation-oriented
handoff for another repository that wants to understand or reuse the design.

## Core contract

The proxy has one critical invariant:

> Forward the request and response bodies unchanged. Replace only the caller's
> gateway credential with the selected provider credential.

It also authenticates the caller, selects one upstream, enforces policy, meters
the returned usage, and records the attempt. It does not translate payloads,
retry a failed upstream, or fail over within a request.

## File map

| Concern | Implementation |
|---|---|
| Process assembly and dependency injection | `backend/cmd/songguo/main.go` |
| HTTP route mounts | `backend/internal/server/server.go` |
| Main HTTP proxy flow | `backend/internal/proxy/proxy.go` |
| WebSocket handshake and raw relay | `backend/internal/proxy/websocket.go` |
| Provider selection | `backend/internal/router/router.go` |
| Path-to-protocol matching and usage interface | `backend/internal/wire/wire.go` |
| OpenAI response/SSE metering | `backend/internal/wire/openai.go` |
| Anthropic response/SSE metering | `backend/internal/wire/anthropic.go` |
| Request-side model/modality sniffing | `backend/internal/meter/meter.go` |
| Usage-to-cost calculation | `backend/internal/pricing/pricing.go` |
| Provider rows to live routing config | `backend/internal/configsvc/manager.go` |
| Validated immutable routing snapshot | `backend/internal/config/config.go`, `backend/internal/config/snapshot.go` |
| Two-phase call ledger | `backend/internal/store/calls.go` |
| Optional async payload parsing | `backend/internal/proxy/pipeline.go` |
| Detailed architecture | `docs/arch-gateway.md` |

## Startup and configuration flow

```text
SQLite provider rows
  -> configsvc.Manager.Reload()
  -> config.Config validation and normalization
  -> immutable config.Snapshot
  -> atomic pointer swap
  -> router reads Current() on every request
```

1. `backend/cmd/songguo/main.go` opens SQLite, creates
   `configsvc.Manager`, constructs `router.Router`, and injects the snapshot,
   store, router, and logger into `proxy.NewHandler`.
2. `backend/internal/configsvc/manager.go` projects stored providers into
   runtime `config.Vendor` values. A provider contains the upstream origin,
   adapter, credential, models, prices, enabled wires, and full endpoint URLs.
3. `Manager.Reload` builds a new validated snapshot and swaps it atomically.
   Existing requests keep their resolved values; later requests see the new
   snapshot without restarting the process.
4. `backend/internal/router/router.go` calls the snapshot function for each
   routing decision, so provider edits take effect immediately.

This is the main reusable config pattern: keep mutable configuration in a
database, but give the request path an immutable in-memory snapshot.

## HTTP request flow

```text
client
  -> /v1/* or /api/v3/*
  -> authenticate gateway key
  -> buffer request body unchanged
  -> create pending call row
  -> classify request and resolve wire/provider
  -> enforce scope, budget, and RPM
  -> build one upstream request
  -> replace credential headers
  -> send exactly one upstream attempt
  -> relay upstream status, headers, and body
  -> sniff usage while relaying
  -> calculate cost and finalize call row
  -> optional capture/async insights
```

### 1. Route entry

`backend/internal/server/server.go` mounts the same proxy handler at:

- `/v1/` for OpenAI/Anthropic-shaped APIs
- `/api/v3/` for Volcengine speech APIs

Clients use native API paths. There is no proxy-specific path containing the
provider name.

### 2. Caller authentication

`handler.ServeHTTP` in `backend/internal/proxy/proxy.go` calls `clientKey`.
It accepts the gateway user key from:

1. `Authorization` (`Bearer <key>` or a raw key)
2. `X-Api-Key`

The key resolves to a stored user, which supplies scope, budget, RPM, and
capture settings.

### 3. Request buffering and ledger start

For normal HTTP, the full request body is read into memory without modification.
A UUID is minted and `Store.CreateCall` inserts a pending `calls` row before
routing and forwarding continue.

This two-phase ledger makes rejected, interrupted, or crashed requests visible:

```text
CreateCall(id, status=pending)
  -> request completes or is denied
FinalizeCall(id, final status/usage/cost/latency)
```

### 4. Route resolution

`handler.resolve` performs two related selections.

First, it chooses candidate providers using the first available selector:

```text
X-Songguo-Provider header
  else JSON body.model
  else all configured providers
```

Second, `resolveWires` calls `wire.Resolve` for each candidate. A wire is a
named protocol entry such as `openai/chat` with:

- one or more path suffixes
- a modality
- a non-streaming usage extractor
- an optional streaming scanner
- an optional zero-cost marker

The longest enabled path suffix wins. Candidates that do not support the
request path are removed unless `allow_unmatched` is enabled.

The router orders remaining candidates by one lexicographic sort key:

1. health — live, then cooling, then dead
2. session stickiness — the vendor that served this session's previous turn
3. lower numeric priority (a strict tier, not a weight)
4. a weighted random draw inside the same priority

Only `targets[0]` is used. The remaining order is not a retry list.

Health sorts above stickiness so a pin can never hold a session on a broken
vendor, and above priority so a primary actually yields to its backup. The
ordering is a permutation of its input — health demotes but never excludes, so
runtime state can never empty a candidate list or manufacture a refusal.

### 5. Policy checks

Before contacting the provider, `ServeHTTP` enforces:

- model scope for requests with a body model
- provider scope for model-less requests
- user budget
- per-user RPM

Gateway denials finalize the pending call row and return an OpenAI-shaped JSON
error. When capture is enabled, the denial request and generated response are
stored with credential headers redacted.

### 6. Upstream URL and credential

The selected wire may have a full configured endpoint such as:

```text
https://example.com/v1/chat/completions
https://example.com/deployments/{model}/chat/completions?api-version=...
```

`buildUpstreamURL` substitutes `{model}` and merges query parameters. If the
endpoint is origin-only, or unmatched passthrough is enabled, `passthroughURL`
keeps the inbound native path.

`buildUpstreamRequest` then:

1. keeps the original method and buffered body bytes
2. copies non-hop-by-hop headers
3. removes `X-Songguo-Provider`
4. removes both possible caller credential headers
5. injects the provider credential according to its adapter

Adapter behavior is implemented by `applyUpstreamAuth`:

| Adapter | Upstream credential |
|---|---|
| `openai-compatible` | `Authorization: Bearer <provider-key>` |
| `anthropic-compatible` | `X-Api-Key: <provider-key>` plus default `Anthropic-Version` |
| `volc-speech` | `X-Api-Key: <provider-key>` |

### 7. Response relay and metering

`handler.forward` sends the upstream status and headers to the caller before
recording the final ledger data.

For a normal response, it copies the body unchanged and passes a read-only copy
to the wire extractor.

For `text/event-stream`, `streamBody`:

- copies each chunk directly to the caller
- flushes each chunk
- tees bytes into the wire's streaming scanner
- optionally tees bytes into capture storage

The wire normalizes provider-specific usage into `wire.Normalized`. Pricing uses
only usage officially reported by the provider. Missing or unparseable usage
records unknown confidence and zero cost; local token estimates are never used
for billing.

Finally, `Store.FinalizeCall` updates the pending row with the provider, wire,
status, normalized usage, cost, latency, TTFT, and stream metadata. Optional
payload parsing and session insights happen afterward and are best-effort.

## WebSocket flow

WebSocket upgrades branch from `ServeHTTP` before HTTP body buffering.
`backend/internal/proxy/websocket.go`:

1. authenticates through the same caller-key path
2. selects candidates by optional provider pin, then path/wire match
3. applies scope, budget, and RPM before hijacking
4. dials one upstream
5. rebuilds the HTTP/1.1 upgrade request with the provider credential
6. relays a non-101 response normally, or returns the upstream 101
7. hijacks the client connection and runs two `io.Copy` loops
8. records duration and bytes when either side closes

WebSocket frames are not parsed by the relay. Some supported speech wires tee
downstream bytes to a separate non-blocking usage meter.

## What another repository should copy

The useful architecture is the separation of five responsibilities:

1. **Config snapshot:** database writes rebuild an immutable, atomically swapped
   routing view.
2. **Router:** selects and orders providers but never performs network I/O.
3. **Wire registry:** owns path matching and protocol-specific usage extraction.
4. **Proxy handler:** owns authentication, policy, credential replacement, and
   byte-transparent relay.
5. **Ledger/insights split:** synchronous code records the minimum call outcome;
   heavier analysis runs afterward and cannot block forwarding.

A minimal implementation should preserve these invariants:

- Never deserialize and reserialize forwarded bodies.
- Strip every possible gateway credential before injecting the provider key.
- Forward one upstream attempt and return its real result.
- Keep usage parsing read-only and non-fatal.
- Treat streaming as a relay with a tee, not as a buffered transformation.
- Resolve configuration once per request from an immutable snapshot.
- Record attempts, including denials and incomplete calls.

## Known tradeoffs

- Normal HTTP request bodies are fully buffered with no size limit. Memory use is
  approximately payload size times concurrent requests.
- Non-streaming responses are also buffered for metering.
- Streaming capture stores the complete stream in memory when capture is enabled.
- Budget enforcement is a coarse pre-check; concurrent calls can cross the cap.
- Routing state (health, session pins) is in-process and is not coordinated
  across replicas.
- Weighted selection is a random draw, so the traffic split is correct in
  expectation rather than exact over short bursts.
- Health demotion is cross-request only: a failing vendor is steered away from
  on the NEXT request. There is still no retry, no per-request failover, and no
  hard ejection — a demoted vendor is ranked last, never removed.
- Failure detection is passive. Nothing probes an idle vendor, so a dead one is
  discovered by a real client request failing.
- WebSocket routing and metering are intentionally less protocol-aware than the
  HTTP path.

These are deliberate simplifications in this single-tenant implementation, not
requirements for a reusable proxy library.
