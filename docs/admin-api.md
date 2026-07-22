# Songguo — Admin API & MCP

> How agents and tools drive Songguo's **control plane**. Companion to `prd.md`
> (product) and `registry.md` (the proxy/data-plane catalogue).

Songguo has two surfaces:

- **Data plane** — the transparent proxy at the native vendor paths (`/v1/*` for
  OpenAI/Anthropic-shaped APIs, `/api/v3/*` for Volcengine speech); the provider is
  selected by the `X-Songguo-Provider` header, else the body's model, else the wire
  default. It speaks each vendor's native wire protocol, so any SDK just points its
  `base_url` at Songguo. The proxy wire itself is **never** wrapped in MCP — that
  would be a body-level translation layer, which Songguo never does. See
  `registry.md`.
- **Control plane** — the admin API under `/api/*`: usage, the call ledger, users,
  providers, services, pricing and settings. This is what the dashboard uses, and
  what the **admin MCP server** (`/admin/mcp`) and the **OpenAPI spec** below expose
  for operators.
- **Services MCP** — a consumer-facing MCP server at the bare `/mcp`, gated by a
  **consumer key**. Its tools are songguo's services organized by **task**
  (Text-to-Image, Text-to-Speech, Automatic Speech Recognition, Text-to-Video);
  each constructs a native vendor request and originates it *through* the proxy
  in-process, so the call is routed, credential-swapped and metered like any
  other — against the caller's key. The translation lives in the tool, above the
  wire, so byte-transparency is preserved. See [Services MCP](#services-mcp) below.

## Auth

Every `/api/*` endpoint and the `/admin/mcp` endpoint are gated by a single admin
bearer key, `SONGGUO_ADMIN_KEY`:

```
Authorization: Bearer <SONGGUO_ADMIN_KEY>
```

If the key is unset the server runs the admin API **unprotected** and logs a
warning (intended for local dev only). The OpenAPI spec at `/openapi.yaml` is
served **without** auth — it describes shapes only and carries no secrets.

## REST endpoints

| Method & path | What it does |
|---|---|
| `GET /api/overview` | Spend & health summary for a window (total spend, by modality, error rate, latency, burn, runway). |
| `GET /api/usage/series` | Cost/request/error totals bucketed by hour or day. |
| `GET /api/calls` | Browse the per-call ledger (filter by user/model/vendor/status/time, paginated). |
| `GET /api/calls/export` | Download filtered calls as CSV or JSON. |
| `GET /api/calls/{id}/trace` | Captured request/response payload for a call (when capture is enabled for that user). Covers gateway-denied calls too — an unmatched `404`, scope `403`, budget `402`, or rate-limit `429` saves the request plus the synthesized error body, so a rejected request is as inspectable as a forwarded one. (Upstream transport/build `502` failures record a row but no payload — there is no served response to pair.) |
| `GET /api/users` · `POST /api/users` | List / create users (keys). Create returns the plaintext key once. |
| `PATCH /api/users/{id}` · `POST /api/users/{id}/revoke` | Update / revoke a user. |
| `GET /api/providers` · `POST /api/providers` | List / create upstream providers. |
| `GET /api/providers/{id}` · `PATCH` · `DELETE` | Get / update / delete a provider. |
| `POST /api/providers/{id}/test` | Probe a provider host for reachability. |
| `GET /api/services` | Auto-derived, model-centric services (each model → the providers behind it). |
| `GET /api/vendors` · `POST /api/vendors/{name}/test` | List snapshot vendors / probe one. |
| `GET /api/catalog` · `GET /api/wires` | Provider presets / registered wire names. |
| `GET /api/settings` | Read runtime settings. |
| `GET /api/pricing` | Flattened per-provider model prices. |

The precise request/response schemas are in the OpenAPI 3.1 spec, served by the
binary and embedded at `backend/internal/api/openapi.yaml`:

```
GET /openapi.yaml    # YAML source
GET /openapi.json    # same document as JSON
```

## Admin MCP server (`/admin/mcp`)

Operators connect an MCP client to the streamable-HTTP endpoint:

```
URL:    http://<host>:12345/admin/mcp
Header: Authorization: Bearer <SONGGUO_ADMIN_KEY>
```

It runs in **stateless** mode in the same binary — no extra process, no session
store. Tools reuse the exact same logic as the REST handlers, so their output
never drifts from the dashboard.

### Tools

**Read (always available)**

| Tool | Reads |
|---|---|
| `get_overview` | spend & health summary (args: `since?`, `until?`) |
| `get_usage_series` | spend trend buckets (args: `since?`, `until?`, `bucket?`) |
| `list_calls` | the call ledger (args: `user_id? model? vendor? status? since? until? limit? offset?`) |
| `get_call_trace` | one captured payload (args: `id`) |
| `list_users` | users + lifetime spend |
| `list_providers` | configured providers (keys masked) |
| `list_services` | auto-derived model → providers |
| `list_pricing` | per-provider model prices |
| `get_settings` | non-secret runtime settings |

**Write (opt-in only)** — registered **only** when `SONGGUO_MCP_WRITE` is set,
because the admin key already controls budgets and upstream credentials and an
agent should not get write access implicitly:

`create_user`, `update_user`, `revoke_user`, `create_provider`,
`update_provider`, `delete_provider`, `test_provider`.

```
# enable agent writes (default: off)
SONGGUO_MCP_WRITE=1 songguo
```

For `create_provider` / `update_provider`, each endpoint is `{ wire, endpoint,
adapter }` — a wire bound to its **full upstream URL** and auth scheme (no
provider-level base URL; see `registry.md`).

## Services MCP

The **consumer-facing** MCP server, at the bare `/mcp`. Where the admin MCP
drives the control plane, the services MCP exposes *capabilities* an agent
invokes. It runs stateless in the same binary.

```
URL:    http://<host>:12345/mcp
Header: Authorization: Bearer <consumer key>    # or  X-Api-Key: <consumer key>
```

Auth is a **consumer key** (the same key an SDK would use to call the proxy),
read from `Authorization: Bearer` or `X-Api-Key` — the same ingress as the data
plane. A missing/invalid/revoked key is a `401` before any tool runs.

Each tool builds a **native vendor request** and originates it *through* the
transparent proxy in-process, carrying the caller's key (and an optional
provider pin). So the call is routed, credential-swapped and metered exactly
like a direct proxy call — against that key. The schema→native translation lives
in the tool, **above** the wire; the proxy still forwards the bytes verbatim, so
byte-transparency holds.

### Tools

Tools are named by **task** — the taxonomy the wires already declare via
`Modality` (`chat`→Text Generation, `image`→Text-to-Image, `tts`→Text-to-Speech,
`stt`→Automatic Speech Recognition, `video`→Text-to-Video). Text Generation and
Feature Extraction stay native (an agent points its SDK at `/v1`); the media
tasks get tools. Async tasks are a **submit + poll** pair — no server-side
waiting or handle; the caller owns the loop, and each call meters independently.

| Task | Tool(s) | Native call (wire) |
|---|---|---|
| Text-to-Image | `text-to-image` | `POST /v1/images/generations` (`openai/images`) |
| Text-to-Speech | `text-to-speech` | `POST /api/v3/tts/unidirectional` (`volc/tts-unidirectional`) |
| Automatic Speech Recognition | `automatic-speech-recognition` → `get-transcription` | `…/auc/bigmodel/submit` → `…/query` (`volc/asr-file`) |
| Text-to-Video | `text-to-video` → `get-text-to-video` | `…/generations/tasks` → `GET …/tasks/{id}` (`ark/video`) |

**Provider affinity** for the submit→poll pairs is the real `X-Songguo-Provider`,
exposed as a `provider` arg — pin the same value on submit and poll when several
providers serve the endpoint. The ASR task key is a client-owned
`X-Api-Request-Id` (submit generates it and hands it back; poll takes it).

**Honesty note:** the Text-to-Video poll (`…/tasks/{id}`) is *not* a metered wire
— `ark/video` only matches the submit suffix, so the status GET is served by
**unmatched passthrough**. It works exactly as it does for a direct proxy caller:
the pinned provider must allow unmatched passthrough, and the poll meters zero.

A tool's failure is returned as an MCP tool error whose text is the
vendor/gateway response verbatim, so the agent can self-correct.
