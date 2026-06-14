# Songguo — Wire Registry

> Reference for everything Songguo can proxy. Companion to `prd.md` (product) — this is the concrete catalogue.

## Lead principle: proxy + track, nothing else

Songguo is a **gate + meter, not a transformer**. For every request it:

- **Mutates exactly one thing** — the credential. It swaps the consumer's Songguo token for the real upstream key (auth adapter per wire, see below).
- **Never touches** the request body, the `model` string, or any other header; never rewrites the response.
- **Reads** the response only to meter usage. For streams it tees the bytes through untouched and observes them in flight.

That means, explicitly:

- **No format translation.** The body that arrives is the body that's forwarded. Consumers use each vendor's native SDK/protocol.
- **No model mapping / aliasing — ever.** The `model` string is matched exactly and passed through verbatim. There is no rename, no group, no 重定向, no 倍率分组.
- **No async→sync conversion.** Submit→poll lifecycles are owned by the consumer; Songguo forwards and meters each call independently.
- **Metering is read-only sniffing.** If a usage shape isn't recognized the call still succeeds (coarse/unknown metering) — **parsing never blocks traffic.**
- **"Quirks" parameterize how usage is _read_, never what is _sent._** e.g. `{"cache_tokens":"deepseek"}` only tells the meter which field holds cached tokens; the forwarded payload is identical.

The only thing Songguo will refuse to forward is an over-budget / out-of-scope call (it _rejects_, it does not _transform_).

## The model: four layers

A **wire** is the protocol contract. A **Songguo endpoint** is its inbound face; a **provider endpoint** is its outbound face; **routing** connects one to the other by an explicit selector — a provider header, else the exact model string, else the wire's default.

| Layer | What it is | Static / dynamic | Cardinality |
|---|---|---|---|
| **Wire** | Protocol shape + metering contract (`openai/chat`). The fixed vocabulary. | Static (compiled-in, 10 today) | the catalogue |
| **Songguo endpoint** | The public path a consumer calls (`POST /v1/chat/completions`). Inbound face of a wire. | Static (matched by suffix) | → exactly 1 wire |
| **Provider endpoint** | An **exact vendor URL** that speaks the same wire (`https://api.openai.com/v1/chat/completions`) + its credential. Outbound face. | Dynamic (operator-set, SQLite) | → exactly 1 wire |
| **Routing** | Given `(wire)` pick the provider endpoint, by `header → model-string → default`. Exact match, no aliasing. | Dynamic (SQLite) | selector → provider |

Request lifecycle, one line:

```
inbound path → match Songguo endpoint (wire) by path suffix
            → select provider: X-Songguo-Provider header ?? body model ?? wire default
            → forward to exact vendor URL, swap auth, body + model unchanged
            → wire meters the response (read-only)
```

### There is no "base URL" concept

Every endpoint — inbound and outbound — is a **full, explicit path**. Songguo never derives multiple endpoints from a base; each wire is its own entry. The `base_url` field that SDKs require survives **only as derived text in connect snippets** (the OpenAI SDK appends `/chat/completions` itself, so its card shows `<origin>/v1`; the Anthropic SDK appends `/v1/messages`, so its card shows `<origin>`). That value is presentation, computed per protocol family — it is never stored and never participates in routing.

### Path matching semantics

Matching is by **path suffix**, scoped to the service's enabled wires:

- Case-insensitive; query string and trailing slashes stripped.
- **Longest matching suffix wins** (`/chat/completions` beats `/completions`); ties break lexicographically by wire name.
- No match → **deny** (unless the service opts into unmatched passthrough).

Because matching is suffix-based, the path _prefix_ is conventional. The canonical endpoints below use each vendor's standard prefix (`/v1/...`); a request to any path ending in the same suffix resolves the same way.

### Provider selection

Every request resolves the same way — there are **no addressing "modes."** Once the wire is fixed by path suffix, the provider is chosen by the first available selector:

1. **`X-Songguo-Provider: <name>` header** — explicit pin. A control header (like `X-Control-Require-Usage-Tokens-Return`): **stripped before forwarding**, never part of the body, so it stays inside no-transform. Use it to pick a specific account/provider, or to keep a submit→poll lifecycle on the same provider (affinity).
2. **The body's `model` string** — for model-bearing wires, picks the provider(s) that declare `(wire, model)`; pooling + failover apply.
3. **The default provider** — when neither a header nor a model is present, every vendor serving the matched wire is a candidate, ordered by the existing priority → weighted-RR → health ranking; the top one is the default and the rest are failover. (No separate "default" flag — it reuses provider priority.)

If none resolves, the call is denied with a clear error.

Two consequences:

- **Paths are always native — there is no `/x/<provider>/` prefix.** A model-less endpoint is reached at its plain vendor path (`GET /v1/models`, `POST /api/v3/tts/unidirectional`); the provider comes from the header or the default, never the path.
- **Bare `GET /v1/models` works** and returns the selected provider's list. That is a passthrough of *one* provider's response — Songguo still never aggregates lists across providers (a merged list would be a synthesized response = transform).

## The registry — everything supported today (10 wires)

One row per wire. **Endpoint** = the native path the consumer calls; **bold** marks the suffix that's actually matched (the prefix is conventional). **Providers** = example vendors that speak the wire — the real set is operator-configured in SQLite, not fixed here. **Routing** = how the provider is picked once the wire is matched (full order is always `header → model → default`; see [Provider selection](#provider-selection)). `exact model` = model-bearing, keyed on the body `model`; `header · default` = model-less, no model step.

| Endpoint | Wire | Providers (examples) | Routing |
|---|---|---|---|
| `POST /v1`**`/chat/completions`** | `openai/chat` | OpenAI, Azure, DeepSeek, MiniMax, … | exact `model` |
| `POST /v1`**`/completions`** | `openai/completions` | OpenAI (legacy), … | exact `model` |
| `POST /v1`**`/embeddings`** | `openai/embeddings` | OpenAI, Azure, … | exact `model` |
| `POST /v1`**`/responses`** | `openai/responses` | OpenAI | exact `model` |
| `GET /v1`**`/models`** | `openai/models` | any OpenAI-compatible | header · default |
| `POST /v1`**`/messages`** | `anthropic/messages` | Anthropic | exact `model` |
| `GET /v1`**`/models`** | `anthropic/models` | Anthropic | header · default |
| `POST /api/v3`**`/tts/unidirectional`** | `volc/tts` | Volcengine | header · default |
| `POST /api/v3`**`/tts/voice_clone`** · `GET /api/v3`**`/tts/get_voice`** | `volc/voice-clone` | Volcengine | header · default |
| `POST /api/v3`**`/auc/bigmodel/submit`** · `POST /api/v3`**`/auc/bigmodel/query`** | `volc/asr` | Volcengine | header · default |

Volcengine speech is model-less: it's reached at its **native Volcengine path** (`/api/v3/...`) and the provider comes from `X-Songguo-Provider` (or the wire default). The suffix is what the wire matches. For `volc/asr`, send the same `X-Songguo-Provider` on both `submit` and `query` so the poll lands on the provider that issued the task.

All wires normalize into one canonical view: `{ InputTokens, OutputTokens, CachedInputTokens, Calls, Images, Seconds, Chars }`. Raw vendor usage is logged verbatim alongside.

## Metering

Read-only by design: if a usage shape isn't recognized the call still succeeds with coarse/unknown metering — parsing never blocks traffic. Per-wire fields the meter sniffs:

- **`openai/chat`, `openai/completions`, `openai/embeddings`** — top-level `usage`: `prompt_tokens`/`input_tokens` + `completion_tokens`/`output_tokens`. Cached input per quirk: default `prompt_tokens_details.cached_tokens`, DeepSeek `prompt_cache_hit_tokens`, MiniMax `cached_tokens`. Streaming usage rides the final SSE chunk (some vendors only when the client sets `stream_options.include_usage`); embeddings is input-only, no stream.
- **`openai/responses`** — `usage.input_tokens` + `output_tokens` + `input_tokens_details.cached_tokens`; streaming usage rides the `response.completed` event under `response.usage`.
- **`anthropic/messages`** — `input_tokens` + `cache_read_input_tokens` + `cache_creation_input_tokens` folded into `InputTokens` (cache-create's 1.25× premium ignored, by design); `cache_read` also recorded as `CachedInputTokens`. Streaming merges `message_start.message.usage` (input) with `message_delta.usage` (output).
- **`volc/tts`** — `usage.text_words` → `Chars` (per-char); streamed as NDJSON, and only returned when the client sets `X-Control-Require-Usage-Tokens-Return`, else coarse/unknown.
- **`volc/asr`** — `audio_info.duration` (ms) → `Seconds` (per-second); the `submit` ack has no `audio_info` (meters zero), the `query` poll bills.
- **`openai/models`, `anthropic/models`, `volc/voice-clone`** — zero-cost management endpoints, not parsed. (Voice-clone's slot fee is billed out-of-band on first synthesis.)

## Auth adapters

Derived from the wire name prefix — the operator never picks it.

| Adapter | Wires | Scheme |
|---|---|---|
| `openai-compatible` | `openai/*` | `Authorization: Bearer <key>` |
| `anthropic-compatible` | `anthropic/*` | `x-api-key: <key>` + `anthropic-version` header |
| `volc-speech` | `volc/*` | `x-api-key: <key>` |

## Resolved decisions

1. **Bare `GET /v1/models` returns one provider's list.** Model-listing carries no model string, so the provider comes from `X-Songguo-Provider` (or the priority-ordered default). The `openai/models`/`anthropic/models` suffix tie-break is resolved by the service's enabled wires (a service holds at most one `/models` wire per family). The response is that single provider's list, forwarded verbatim — Songguo never aggregates lists across providers (a merged list would be a synthesized response = transform).
2. **Volcengine paths are the native `/api/v3/...`** with no Songguo-local prefix; the provider comes from `X-Songguo-Provider` / the default (speech is model-less). Suffix matching is pinned by `wire/volc_test.go`.

## Implementation status

- **Full per-wire endpoints — done.** Provider config stores an explicit full upstream URL per wire (DB column `provider_endpoints.endpoint`), used as-is — no base+suffix join. `{model}` in the path is substituted with the request's model, and an endpoint query (e.g. Azure's `?api-version=…`) is merged with any inbound query, so non-uniform vendors like **Azure OpenAI** (`/openai/deployments/{model}/chat/completions?api-version=…`) work. Model-less / WebSocket forwarding uses the vendor's `origin` (scheme://host) with the inbound native path. Runtime vendors group by `(origin, adapter)`. A one-time idempotent migration renames `base_url`→`endpoint` and rewrites legacy bases to full URLs.
- **Unified addressing — done.** One resolution path: match the wire by suffix, then select the provider `header → model → default`. `X-Songguo-Provider` (provider id) is a control header, stripped before forwarding. The default reuses provider priority — no separate flag. The `/x/<provider>/` passthrough is **removed**; the proxy is mounted at the native prefixes `/v1/` and `/api/v3/` (the latter is more specific than the admin `/api/`, so ServeMux routes it to the proxy). WebSocket upgrades carry the pin in the same header. `router.Candidates`/`CandidatesForProvider`/`AllCandidates` back the three selectors.
- **Still open:** `prd.md` §4.1 still models `Channel.base_url`; "Channel" (PRD) ≈ "provider"/"vendor" (config) should be reconciled when the PRD is next revised. A new native top-level path prefix (beyond `/v1/`, `/api/v3/`) would need an added proxy mount in `server.go`.

