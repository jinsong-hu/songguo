# Songguo 松果

> Bring transparency to AI usage.

A self-hosted, single-tenant gateway that sits in front of every AI provider you use and does exactly three things: **auth, billing, observability**. It **never rewrites your traffic** — it swaps the credential, meters the call, enforces the budget, and forwards everything else untouched. One binary, your keys, your budgets, your data.

See [docs/prd.md](docs/prd.md) for the full product thinking.

---

## Why

You call many providers across many modalities, often through several resellers (号池) at once. You want one place to manage keys + budgets, see real spend, and not wake up to a runaway bill — without running a multi-tenant reseller platform or a service mesh.

Songguo is **single-tenant, multi-token**: one owner, many scoped keys. No accounts, no payment rails, no request translation. Because it's **transparent** (forwards bodies verbatim), new vendor models and fields work day one — there's no per-vendor request/response mapping to maintain.

## Features (v1)

- **Transparent proxy** for OpenAI-compatible APIs (chat, embeddings), including **SSE streaming** — forwarded chunk-by-chunk, never buffered.
- **One addressing model** — native vendor paths (`/v1/...`, `/api/v3/...`) with the provider selected by the `X-Songguo-Provider` header, else the body's model, else the wire default. Covers OpenAI-compatible SDKs, native/rerank, and model-less submit→poll APIs alike — no `/x/` mount (see below).
- **号池 routing + failover** — multiple vendors per model and multiple credentials per vendor; priority → weighted round-robin, credential rotation, automatic failover on `429`/`5xx`.
- **Budgets & scope** per token (hard cap, allowed models, per-token RPM). Over-budget calls are rejected, not transformed.
- **Read-only metering** — usage sniffed from the native payload (with coarse fallback); a parse failure never blocks traffic. Per-model pricing yields true cost.
- **Append-only call log** — one row per call attempt; every dashboard view is a query over it.
- **WebSocket passthrough** — realtime APIs (OpenAI Realtime, streaming ASR/TTS) proxied at the native path with an `X-Songguo-Provider` pin: the handshake is replayed with the credential swapped, frames are piped untouched, and the session is metered by bytes + duration.
- **Opt-in request/response capture** — store the raw request + response bodies (headers redacted, size-capped, retained) and inspect them by expanding a call in the dashboard. Off by default; per-token override.
- **Dashboard** (light + dark, pine-green) — Overview (spend, runway, by-modality, latency percentiles, recent calls with filters + CSV/JSON export, expand a row to view its captured request/response), **Services** (manage upstreams: keys, models, prices, health/connectivity test), **Catalog** (browse known providers and add one in a click), Tokens (CRUD with budget bars), Settings.
- **Vendor config in SQLite, managed from the dashboard** — add/edit services, rotate keys, and set prices on the **Services**/**Catalog** pages; changes apply immediately with no restart.
- **One binary.** Pure-Go SQLite (no cgo), the dashboard embedded via `go:embed`.

## Quickstart

```bash
# 1. Build (compiles the dashboard, then the single ./songguo binary)
make build

# 2. Run
export SONGGUO_ADMIN_KEY="$(openssl rand -hex 16)"   # gates the dashboard + admin API
./songguo
# -> songguo listening on :8080

# 3. Add a service in the dashboard
#    Open http://localhost:8080/, go to Catalog, pick a provider, paste your API key.
```

The dashboard is also pre-built and committed (in `backend/web/dist`), so if you already have it built you can compile the binary with Go alone: `cd backend && go build -o ../songguo ./cmd/songguo`.

For local development, run `make dev` and open **http://localhost:5173** — this starts the Go backend on `:8080` and the Vite dev server on `:5173` (which proxies API traffic to the backend); Ctrl+C stops both.

Open the dashboard at **http://localhost:8080/** (production) or **http://localhost:5173/** (dev) and enter the admin key. Mint a token on the **Tokens** page, then point any OpenAI-compatible SDK at the gateway, using that token as the API key:

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sg-...")  # your Songguo token
client.chat.completions.create(model="gpt-4o", messages=[{"role": "user", "content": "hi"}])
```

The call is forwarded to whichever vendor serves `gpt-4o`, metered into the call log, and counted against the token's budget.

## Configuration

Services (an upstream's adapter, base URL, credential, and per-model prices) and tokens both live in **SQLite** and are managed from the dashboard — the **Services** page to add/edit them by hand, the **Catalog** page to add a known provider in one click. A new service speaks one of two adapters: `openai-compatible` (bearer auth) or `anthropic-compatible` (`x-api-key` + `anthropic-version`).

### Addressing

Songguo's transparent proxy has **one resolution rule** — match the wire by path suffix, then select the provider — applied to every request. Consumers call **native vendor paths** (OpenAI/Anthropic-shaped APIs under `/v1/...`, Volcengine speech under `/api/v3/...`); there is no `/x/` mount. The provider is chosen by the first available selector:

- **`X-Songguo-Provider` header** — an explicit pin (by provider id). It is a control header, stripped before forwarding. Use it to target a specific account, or to keep an async **submit→poll** lifecycle on one provider.
- **the body's `model` string** — routed across every vendor that serves it (priority → weighted RR → failover).
- **the wire's default provider** — used when there is neither a header nor a model (e.g. a model-less `GET /v1/models` or `POST /api/v3/tts/unidirectional`).

This makes model-less calls — model listings, the `volc/*` speech wires, and async submit→poll flows — first-class without a special path. **WebSocket upgrades** (realtime APIs) work the same way: they carry no body, so the caller pins the provider with `X-Songguo-Provider`; the handshake is replayed with the credential swapped and frames are piped untouched, metered by bytes + duration.

**Endpoint convention:** each enabled wire stores a full upstream URL (a `{model}` placeholder substituted, its query merged), so non-uniform vendors work as-is — Ark/方舟 `https://ark.cn-beijing.volces.com/api/v3/chat/completions`, DashScope/百炼 `https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions`, Azure's `…/deployments/{model}/chat/completions?api-version=…`. Model-less and WebSocket forwarding uses the vendor's `scheme://host` origin with the inbound native path.

### Request/response capture (optional)

Off by default. Toggle capture from the dashboard **Settings** page to record the raw request + response bodies for each call — view them by expanding a call row in the dashboard. Bodies are size-capped (`capture_max_bytes`, default 32 KB) and pruned to the newest `capture_retain` rows (default 10000); captured headers are redacted (no `Authorization`). A token can override the global setting (`capture: true|false`). WebSocket sessions record metadata only (bytes/duration), not frames.

### Environment variables

| Var | Default | Purpose |
|-----|---------|---------|
| `SONGGUO_LISTEN` | `:8080` | Listen address |
| `SONGGUO_DB` | `./songguo.db` | SQLite path (auto-migrated); source of truth for services + tokens |
| `SONGGUO_ADMIN_KEY` | _(empty)_ | Admin/dashboard key. **If empty, the admin API is unprotected** (a warning is logged). |

### Auth model

- **Admin / dashboard / `/api`** → the single `SONGGUO_ADMIN_KEY`, sent as `Authorization: Bearer <key>`.
- **Proxy traffic / `/v1`** → the per-app **tokens** you mint in the dashboard, sent by the consumer's SDK as its API key.

## Routes

| Path | Purpose |
|------|---------|
| `/v1/*` | Transparent proxy, native OpenAI/Anthropic-shaped paths (consumer traffic) |
| `/api/v3/*` | Transparent proxy, native Volcengine speech paths (model-less / async / **WebSocket**) |
| `/api/*` | Admin REST API (admin-key gated) |
| `/` | Embedded dashboard |
| `/healthz` | Liveness |

## Architecture

One binary, single-tenant, SQLite by default, near-zero ops. The call log is the spine — an append-only table of sniffed calls. Vendor/service config lives in SQLite too; the gateway holds a live in-memory snapshot rebuilt on every dashboard write, so key/model/price changes apply with no restart. The only mutation Songguo makes to a forwarded request is swapping in the upstream credential (per the service's adapter).

```
backend/
  cmd/songguo   main entrypoint
  internal/
    config/    config types + validation, projected into the routing snapshot
    configsvc/  builds the live snapshot from SQLite service rows
    catalog/    embedded read-only preset directory (providers/services/models)
    store/      SQLite: services + credentials + prices, call log, tokens, aggregations
    calls/      call-log domain types
    pricing/    usage + price table -> cost
    meter/      read-only modality/usage sniffing (JSON + SSE)
    router/     号池 candidate selection, weighted RR, health/failover
    proxy/      the transparent /v1 + /x handler (adapter-aware auth)
    api/        admin /api handlers
    server/     HTTP wiring (proxy, api, dashboard, health)
  web/        embeds the built dashboard (web/dist) via go:embed
frontend/     React + Vite dashboard source (built into backend/web/dist)
Makefile      dev / build orchestration
```

## Development

```bash
# Run backend (:8080) + Vite dev server (:5173) together; Ctrl+C stops both.
# Vite proxies /api, /v1, /x, /healthz to the backend. Open http://localhost:5173
make dev

# Build the dashboard into backend/web/dist (embedded), then the ./songguo binary
make build

# Tests
make test            # cd backend && go test ./...
```

The dashboard build output goes to `backend/web/dist`, which is committed so the Go binary builds without Node. After frontend changes, run `make build` (or `cd frontend && npm run build`) and commit the refreshed `backend/web/dist`.

## Not in v1 (deferred)

Async multimodal channels (image/video submit→poll) are **forwardable** via native vendor paths (provider pinned with `X-Songguo-Provider`), and **realtime WebSocket** APIs are proxied (metered by bytes + duration). Still deferred: **per-image / per-second media metering** (model-less media + WebSocket calls are forwarded and recorded, but dollar cost is only computed when the vendor returns token usage), **vendor-specific WebSocket auth** (the WS handshake swaps the `Authorization: Bearer` header — providers that authenticate realtime via signed URLs / query-param tokens need per-vendor handling), AK/SK request signing, MCP tool proxying, tag-based business attribution, and cross-model cost×latency optimization. The calls schema already carries the fields these will use.
