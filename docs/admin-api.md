# Songguo — Admin API & MCP

> How agents and tools drive Songguo's **control plane**. Companion to `prd.md`
> (product) and `registry.md` (the proxy/data-plane catalogue).

Songguo has two surfaces:

- **Data plane** — the transparent proxy at the native vendor paths (`/v1/*` for
  OpenAI/Anthropic-shaped APIs, `/api/v3/*` for Volcengine speech); the provider is
  selected by the `X-Songguo-Provider` header, else the body's model, else the wire
  default. It speaks each vendor's native wire protocol, so any SDK just points its
  `base_url` at Songguo. This is already agent-friendly and is **not** wrapped in
  MCP (that would be a translation layer, which Songguo never does). See
  `registry.md`.
- **Control plane** — the admin API under `/api/*`: usage, the call ledger, users,
  providers, services, pricing and settings. This is what the dashboard uses, and
  what the **MCP server** and the **OpenAPI spec** below expose for agents.

## Auth

Every `/api/*` endpoint and the `/mcp` endpoint are gated by a single admin bearer
key, `SONGGUO_ADMIN_KEY`:

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
| `GET /api/calls/{id}/trace` | Captured request/response payload for a call (when capture is on). |
| `GET /api/users` · `POST /api/users` | List / create users (keys). Create returns the plaintext key once. |
| `PATCH /api/users/{id}` · `POST /api/users/{id}/revoke` | Update / revoke a user. |
| `GET /api/providers` · `POST /api/providers` | List / create upstream providers. |
| `GET /api/providers/{id}` · `PATCH` · `DELETE` | Get / update / delete a provider. |
| `POST /api/providers/{id}/test` | Probe a provider host for reachability. |
| `GET /api/services` | Auto-derived, model-centric services (each model → the providers behind it). |
| `GET /api/vendors` · `POST /api/vendors/{name}/test` | List snapshot vendors / probe one. |
| `GET /api/catalog` · `GET /api/wires` | Provider presets / registered wire names. |
| `GET /api/settings` · `PATCH /api/settings` | Read / update capture settings. |
| `GET /api/pricing` | Flattened per-provider model prices. |

The precise request/response schemas are in the OpenAPI 3.1 spec, served by the
binary and embedded at `backend/internal/api/openapi.yaml`:

```
GET /openapi.yaml    # YAML source
GET /openapi.json    # same document as JSON
```

## MCP server

Agents connect an MCP client to the streamable-HTTP endpoint:

```
URL:    http://<host>:8080/mcp
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
`update_provider`, `delete_provider`, `update_settings`, `test_provider`.

```
# enable agent writes (default: off)
SONGGUO_MCP_WRITE=1 songguo
```

For `create_provider` / `update_provider`, each endpoint is `{ wire, endpoint,
adapter }` — a wire bound to its **full upstream URL** and auth scheme (no
provider-level base URL; see `registry.md`).
