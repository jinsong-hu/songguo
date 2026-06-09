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
- **Two routing modes** — model-routed `/v1/...` for OpenAI-compatible SDKs, and explicit-vendor passthrough `/x/<vendor>/...` for native, rerank, and async submit→poll APIs (see below).
- **号池 routing + failover** — multiple vendors per model and multiple credentials per vendor; priority → weighted round-robin, credential rotation, automatic failover on `429`/`5xx`.
- **Budgets & scope** per token (hard cap, allowed models, per-token RPM). Over-budget calls are rejected, not transformed.
- **Read-only metering** — usage sniffed from the native payload (with coarse fallback); a parse failure never blocks traffic. Per-model pricing yields true cost.
- **Append-only call log** — one row per call attempt; every dashboard view is a query over it.
- **WebSocket passthrough** — realtime APIs (OpenAI Realtime, streaming ASR/TTS) proxied over `/x/<vendor>/`: the handshake is replayed with the credential swapped, frames are piped untouched, and the session is metered by bytes + duration.
- **Opt-in request/response capture** — store the raw request + response bodies (headers redacted, size-capped, retained) and inspect them by expanding a call in the dashboard. Off by default; per-token override.
- **Dashboard** (light + dark, pine-green) — Overview (spend, runway, by-modality, latency percentiles, recent calls with filters + CSV/JSON export, expand a row to view its captured request/response), Vendors (health + connectivity test), Tokens (CRUD with budget bars), Settings.
- **File-based vendor config with hot-reload** — edit `config.yaml`, changes apply with no restart (inotify, with an automatic polling fallback).
- **One binary.** Pure-Go SQLite (no cgo), the dashboard embedded via `go:embed`.

## Quickstart

```bash
# 1. Build (the dashboard is pre-built and committed, so Go alone is enough)
go build -o songguo ./cmd/songguo

# 2. Configure your vendors
cp config.example.yaml config.yaml   # then edit: base_url, credentials, served_models, prices

# 3. Run
export SONGGUO_ADMIN_KEY="$(openssl rand -hex 16)"   # gates the dashboard + admin API
./songguo
# -> songguo listening on :8080
```

Open the dashboard at **http://localhost:8080/** and enter the admin key. Mint a token on the **Tokens** page, then point any OpenAI-compatible SDK at the gateway, using that token as the API key:

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sg-...")  # your Songguo token
client.chat.completions.create(model="gpt-4o", messages=[{"role": "user", "content": "hi"}])
```

The call is forwarded to whichever vendor serves `gpt-4o`, metered into the call log, and counted against the token's budget.

## Configuration

Vendors, credentials, and prices live in `config.yaml` (source of truth, hot-reloaded). Tokens live in SQLite and are managed from the dashboard. See [config.example.yaml](config.example.yaml) for the annotated schema.

```yaml
vendors:
  - name: openai-main
    base_url: https://api.openai.com/v1   # vendor's OpenAI-compatible base, INCLUDING any version/path prefix
    served_models: [gpt-4o, text-embedding-3-small]
    priority: 1                            # lower = preferred
    weight: 1                              # weighted round-robin within a priority
    credentials:                           # the 号池 — rotated to spread per-key limits
      - { id: openai-key-1, api_key: sk-... }
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
```

### Routing modes

Songguo's transparent proxy serves two shapes from one handler, chosen by the request path:

- **Model-routed (`/v1/...`)** — the ergonomic default. Point any OpenAI-compatible SDK at `http://<songguo>/v1`; the model is read from the body and routed across every vendor that serves it (priority → weighted RR → credential rotation → failover). The upstream URL is `base_url + (path after /v1)`.
- **Explicit passthrough (`/x/<vendor>/...`)** — pin a vendor by name and forward the rest of the path to its **host** (`base_url`'s `scheme://host`, path stripped). **No model is required**, which makes DashScope's native generation endpoints and async image/video **submit→poll** flows forwardable (e.g. `POST /x/bailian/api/v1/services/aigc/.../generation`, then `GET /x/bailian/api/v1/tasks/{id}`). **WebSocket upgrades on this path are proxied too** (realtime APIs): the handshake is replayed with the credential swapped and frames are piped untouched, metered by bytes + duration. Failover here is across that vendor's **own credentials** only; a scoped token may be limited to specific vendors.

**`base_url` convention:** it is the vendor's *published* base **including any version/path prefix** — OpenAI `https://api.openai.com/v1`, Ark/方舟 `https://ark.cn-beijing.volces.com/api/v3`, DashScope/百炼 `https://dashscope.aliyuncs.com/compatible-mode/v1`, DeepSeek `https://api.deepseek.com`. So a model-routed `/v1/chat/completions` reaches `…/api/v3/chat/completions` on Ark, while a passthrough `/x/bailian/api/v1/tasks/abc` reaches `https://dashscope.aliyuncs.com/api/v1/tasks/abc`.

### Request/response capture (optional)

Off by default. Set `settings.capture: true` in `config.yaml` to record the raw request + response bodies for each call — view them by expanding a call row in the dashboard. Bodies are size-capped (`capture_max_bytes`, default 32 KB) and pruned to the newest `capture_retain` rows (default 10000); captured headers are redacted (no `Authorization`). A token can override the global setting (`capture: true|false`). WebSocket sessions record metadata only (bytes/duration), not frames.

### Environment variables

| Var | Default | Purpose |
|-----|---------|---------|
| `SONGGUO_LISTEN` | `:8080` | Listen address |
| `SONGGUO_CONFIG` | `./config.yaml` | Vendor config file (hot-reloaded) |
| `SONGGUO_DB` | `./songguo.db` | SQLite path (auto-migrated) |
| `SONGGUO_ADMIN_KEY` | _(empty)_ | Admin/dashboard key. **If empty, the admin API is unprotected** (a warning is logged). |

### Auth model

- **Admin / dashboard / `/api`** → the single `SONGGUO_ADMIN_KEY`, sent as `Authorization: Bearer <key>`.
- **Proxy traffic / `/v1`** → the per-app **tokens** you mint in the dashboard, sent by the consumer's SDK as its API key.

## Routes

| Path | Purpose |
|------|---------|
| `/v1/*` | Transparent proxy, model-routed (consumer traffic) |
| `/x/<vendor>/*` | Transparent proxy, explicit-vendor passthrough (native / async / rerank / **WebSocket**) |
| `/api/*` | Admin REST API (admin-key gated) |
| `/` | Embedded dashboard |
| `/healthz` | Liveness |

## Architecture

One binary, single-tenant, SQLite by default, near-zero ops. The call log is the spine — an append-only table of sniffed calls. The gateway holds a live in-memory view of the file config so vendor/key/model/price changes apply without a restart. The only mutation Songguo makes to a forwarded request is swapping in the upstream credential.

```
internal/
  config/   file-based vendor config + hot-reload (watch or poll)
  store/    SQLite: append-only call log + tokens + aggregations
  calls/    call-log domain types
  pricing/  usage + price table -> cost
  meter/    read-only modality/usage sniffing (JSON + SSE)
  router/   号池 candidate selection, weighted RR, health/failover
  proxy/    the transparent /v1 handler
  api/      admin /api handlers
  server/   HTTP wiring (proxy, api, dashboard, health)
web/        React + Vite dashboard (built into dist/, embedded)
cmd/songguo main entrypoint
```

## Development

```bash
# Rebuild the dashboard after frontend changes (then commit web/dist)
cd web && npm install && npm run build && cd ..

go build ./...
go test ./...        # add -race for the full check
go run ./cmd/songguo
```

In dev you can run the Vite dev server (`cd web && npm run dev`) which proxies `/api` and `/v1` to a locally running `songguo`.

## Not in v1 (deferred)

Async multimodal channels (image/video submit→poll) are **forwardable** via `/x/<vendor>/...` passthrough, and **realtime WebSocket** APIs are proxied (metered by bytes + duration). Still deferred: **per-image / per-second media metering** (passthrough media + WebSocket calls are forwarded and recorded, but dollar cost is only computed when the vendor returns token usage), **vendor-specific WebSocket auth** (the WS handshake swaps the `Authorization: Bearer` header — providers that authenticate realtime via signed URLs / query-param tokens need per-vendor handling), AK/SK request signing, MCP tool proxying, tag-based business attribution, and cross-model cost×latency optimization. The calls schema already carries the fields these will use.
