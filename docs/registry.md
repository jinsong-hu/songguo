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
| **Wire** | Protocol shape + metering contract (`openai/chat`). The fixed vocabulary. | Static (compiled-in, 11 today) | the catalogue |
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
2. **The body's `model` string** — for model-bearing wires, picks the provider(s) that declare `(wire, model)`; pooling applies (health → sticky → priority → weight), and the top candidate is forwarded to.
3. **The default provider** — when neither a header nor a model is present, every vendor serving the matched wire is a candidate, ordered by the same health → sticky → priority → weight ranking; the top one is the default. (No separate "default" flag — it reuses provider priority.)

Only the top candidate is forwarded to: songguo makes **one attempt** per request and surfaces the vendor's response verbatim. There is no per-call retry or failover — the remaining candidates are a ranked pool, not a replay list.

After provider selection, that provider's connection route is applied. The
route is either **Direct** or one reusable HTTPS/SOCKS5 proxy configured in
Settings. Direct is explicit and does not inherit `HTTP_PROXY`, `HTTPS_PROXY`,
or `ALL_PROXY` from the process environment. The same route is used for normal
HTTP requests, WebSocket handshakes, and provider connectivity probes.

Ranking is **health → sticky session → priority → weight**.

A session pins to the provider that served its previous turn, so an agent conversation keeps one vendor and its prompt cache stays warm — on a large context that is the most expensive routing decision songguo makes. Health sorts above the pin, so it is only ever consulted among vendors of equal health and can never strand a session on a broken provider, and a client that sends no session header simply gets the ordinary ordering. Within a priority tier, selection is a **weighted random draw** rather than a rotation: correct in expectation, stateless, and approximate over short bursts. The draw is taken once per **credential**, so a provider split across several protocol endpoints still gets a single share of traffic rather than one per endpoint.

Provider `priority` and `weight` are the defaults for model-less requests and
for every declared model. A `(model, provider)` relationship may override both
values or disable that provider for only that model. Lower numeric priority is
a strict failover tier; weight is a proportional share within one tier. A
service-specific disable also applies to an explicit provider pin when that
request carries the model, but does not disable the provider's other models or
its model-less submit/poll endpoints.

`weight: 0` **parks** a provider: no share of its tier, so no new session lands
there while a weighted provider shares its priority — but it stays configured and
a full candidate. An explicit provider pin still reaches it, a `(model, provider)`
weight override can still give it a share of one service, and sessions already
pinned to it keep it (weight decides where a *new* session lands). Because
parking is a share of zero rather than a filter, it obeys the tier like any other
weight: a parked provider alone in the winning tier still serves. Disabling is the
lever that stops traffic immediately, at the cost of a cold prompt cache for every
live session.

Health is learned passively from real requests — songguo never sends probe traffic.

Failures are graded by the question *"would an identical retry fail identically?"*:

| signal | strikes | outcomes |
|---|---|---|
| `neutral` | 0 | 400/404/408/422, client aborted mid-stream — the caller's fault; every vendor would reject identically |
| `fail` | 1 (3 demote) | timeout, connection reset, unexpected EOF, temporary DNS failure, 5xx, 403 |
| `fail_model` | demotes `(vendor, model)` only | 429 — a per-model quota, so it never touches the vendor's other models |
| `fail_hard` | demotes at once | connection refused, DNS NXDOMAIN, bad TLS certificate — properties of the endpoint, not the request |
| `fail_credential` | demotes at once, **all sibling vendors** | 401 — a revoked key is dead on every host presenting it |

A transport failure has no status code, so classification inspects the error value (`ECONNREFUSED`, `DNSError.IsNotFound`, TLS verification errors) rather than the status. 403 stays ambiguous on purpose — it can mean "model not on your plan", which is per-model, not vendor-wide.

Scope matters because one provider becomes several vendors via the `(origin, adapter)` split. Those hosts fail independently, so a dead origin never demotes its sibling — but they share one credential, so a 401 demotes all of them at once.

The failure streak **survives the cooldown**: a vendor that fails its first request back is re-demoted immediately, so a permanently dead vendor costs one client-visible failure per window rather than three. Only a success clears it.

Demotion is **cross-request** — it changes which vendor the *next* request goes to, never the one that failed — and it **never excludes**: a cooling vendor still serves if nothing healthier is available, so health can never empty a candidate list. `GET /api/vendors` exposes the live state under `routing`.

If none resolves, the call is denied with a clear error.

Two consequences:

- **Paths are always native — there is no `/x/<provider>/` prefix.** A model-less endpoint is reached at its plain vendor path (`GET /v1/models`, `POST /api/v3/tts/unidirectional`); the provider comes from the header or the default, never the path.
- **Bare `GET /v1/models` works** and returns the selected provider's list. That is a passthrough of *one* provider's response — Songguo still never aggregates lists across providers (a merged list would be a synthesized response = transform).

## The registry — everything supported today (11 wires)

One row per wire. **Endpoint** = the native path the consumer calls; **bold** marks the suffix that's actually matched (the prefix is conventional). **Providers** = example vendors that speak the wire — the real set is operator-configured in SQLite, not fixed here. **Routing** = how the provider is picked once the wire is matched (full order is always `header → model → default`; see [Provider selection](#provider-selection)). `exact model` = model-bearing, keyed on the body `model`; `header · default` = model-less, no model step.

| Endpoint | Wire | Providers (examples) | Routing |
|---|---|---|---|
| `POST /v1`**`/chat/completions`** | `openai/chat` | OpenAI, Azure, DeepSeek, MiniMax, … | exact `model` |
| `POST /v1`**`/completions`** | `openai/completions` | OpenAI (legacy), … | exact `model` |
| `POST /v1`**`/embeddings`** | `openai/embeddings` | OpenAI, Azure, … | exact `model` |
| `POST /v1`**`/responses`** | `openai/responses` | OpenAI | exact `model` |
| `GET /v1`**`/models`** | `openai/models` | any OpenAI-compatible | header · default |
| `POST /v1`**`/messages`** | `anthropic/messages` | Anthropic | exact `model` |
| `POST /v1`**`/messages/count_tokens`** | `anthropic/count_tokens` | Anthropic | exact `model` |
| `GET /v1`**`/models`** | `anthropic/models` | Anthropic | header · default |
| `POST /api/v3`**`/tts/unidirectional`** | `volc/tts` | Volcengine | header · default |
| `POST /api/v3`**`/tts/voice_clone`** · `GET /api/v3`**`/tts/get_voice`** | `volc/voice-clone` | Volcengine | header · default |
| `POST /api/v3`**`/auc/bigmodel/submit`** · `POST /api/v3`**`/auc/bigmodel/query`** | `volc/asr` | Volcengine | header · default |

Volcengine speech is model-less: it's reached at its **native Volcengine path** (`/api/v3/...`) and the provider comes from `X-Songguo-Provider` (or the wire default). The suffix is what the wire matches. For `volc/asr`, send the same `X-Songguo-Provider` on both `submit` and `query` so the poll lands on the provider that issued the task.

All wires normalize into one canonical token view. Raw vendor usage is logged
verbatim alongside. The token fields are **Anthropic-shaped and disjoint** — other
vendors are mapped onto this shape best-effort:

| field (DB column) | meaning |
|---|---|
| `input_tokens` | fresh (uncached) input tokens |
| `cache_read_input_tokens` | cache-read input tokens |
| `cache_creation_input_tokens` | cache-write input tokens |
| `output_tokens` | total output tokens |
| `thinking_tokens` | reasoning/thinking tokens |

The three input-side fields are **disjoint** and sum to the total input
(`input + cache_read + cache_creation`). `thinking_tokens` is a **subset of**
`output_tokens` (never added on top of a total). Cost bills fresh input and cache
creation at the input rate and cache reads at the cached rate — algebraically
identical to the old folded math, so redefining `input_tokens` to fresh-only changed
no invoice. The non-token counters `{ Calls, Images, Seconds, Chars }` are unchanged.

**Cross-vendor mapping.** Anthropic reports the three input fields disjointly, so
they map straight through. OpenAI-style vendors report a cache-*inclusive*
`prompt_tokens`/`input_tokens`, so `input_tokens = prompt_tokens − cached` (clamped
≥ 0) and they carry no cache-creation (→ 0). Reasoning tokens
(`completion_tokens_details.reasoning_tokens` / `output_tokens_details.reasoning_tokens`)
map to `thinking_tokens`, as does Anthropic's `output_tokens_details.thinking_tokens`.

## Metering

Read-only by design: if a usage shape isn't recognized the call still succeeds with coarse/unknown metering — parsing never blocks traffic. Per-wire fields the meter sniffs:

- **`openai/chat`, `openai/completions`, `openai/embeddings`** — top-level `usage`: `prompt_tokens`/`input_tokens` + `completion_tokens`/`output_tokens`. Cached input per quirk: default `prompt_tokens_details.cached_tokens`, DeepSeek `prompt_cache_hit_tokens`, MiniMax `cached_tokens`. `prompt_tokens` is cache-inclusive, so `input_tokens = prompt_tokens − cached` (fresh); no cache-creation (→ 0). `completion_tokens_details.reasoning_tokens` → `thinking_tokens`. Streaming usage rides the final SSE chunk (some vendors only when the client sets `stream_options.include_usage`); embeddings is input-only, no stream.
- **`openai/responses`** — `usage.input_tokens` (cache-inclusive) + `output_tokens` + `input_tokens_details.cached_tokens`; `input_tokens = input_tokens − cached` (fresh), `output_tokens_details.reasoning_tokens` → `thinking_tokens`. Streaming usage rides the `response.completed` event under `response.usage`.
- **`anthropic/messages`** — the reference shape: `input_tokens` (already fresh) + `cache_read_input_tokens` + `cache_creation_input_tokens` mapped straight through as three disjoint fields (cache-create's 1.25× premium ignored, by design); `output_tokens_details.thinking_tokens` → `thinking_tokens`. Streaming merges `message_start.message.usage` (input) with `message_delta.usage` (output).
- **`volc/tts`** — `usage.text_words` → `Chars` (per-char); streamed as NDJSON, and only returned when the client sets `X-Control-Require-Usage-Tokens-Return`, else coarse/unknown.
- **`volc/asr`** — `audio_info.duration` (ms) → `Seconds` (per-second); the `submit` ack has no `audio_info` (meters zero), the `query` poll bills.
- **`anthropic/count_tokens`** — zero-cost: Anthropic bills token counting as free, so the call is logged (for observability) but never priced; the response (`{"input_tokens":N}`, no `usage` object) is not parsed.
- **`openai/models`, `anthropic/models`, `volc/voice-clone`** — zero-cost management endpoints, not parsed. (Voice-clone's slot fee is billed out-of-band on first synthesis.)

### Unpriced models fall back to the provider's ceiling

Metering answers "how much was used"; pricing answers "at what rate". They fail
differently, and only the first defaults to zero.

**Unknown usage meters $0.** If the vendor omits usage or the shape isn't
recognized, the call bills nothing. We bill what the vendor reported, verbatim,
and never substitute a local token count.

**An unknown *price* does not.** A model a provider serves but nobody published
a rate for is given, at config-build time, **the most expensive rate that same
provider charges in the same unit family**. Otherwise a newly released model
(one not yet in `catalog.json`) silently bills $0 until someone notices — an
under-bill indistinguishable from a free call in the ledger. The substitution:

- **stays inside a unit family** — `per_1m_tokens`/`per_1k_tokens`/`per_token`
  are interchangeable after normalizing; `per_call`, `per_image`, `per_second`
  and `per_char` each stand alone. Crossing families would not over-bill, it
  would compute $0 while appearing to carry a rate, because the quantities are
  disjoint.
- **copies a whole real price**, never a per-field maximum. `cached_input: 0`
  means "no discount, charge full input", so a field-wise max would pick the
  *largest discount* and undercut a real model on cache-heavy traffic.
- **never crosses providers.** A provider with no usable same-family rate keeps
  its $0 and is warned about. Borrowing globally would make one provider's bill
  a function of unrelated config.
- **triggers on provenance, not on the number.** Only a model nobody ever
  stated a rate for is eligible. A zero *someone published* is a real price
  meaning free — the catalog lists genuinely free tiers (`glm-4.7-flash`), and
  an operator `price_override` of zero is a deliberate "don't bill this" — and
  is left alone. Re-pricing those would invent a charge for a free model.
- **skips implausibly scaled rates** (a `per_token` value that normalizes above
  $1000/1M is almost certainly a unit typo), so one mistake can't become the
  ceiling for a whole provider.

Every price carries a `source` (`catalog`, `override`, `stored`, `unpriced`, or
`fallback:<model>`), visible in `GET /api/pricing` and the vendor view, so a
borrowed rate is never mistaken for a published one and the ledger can always be
reconciled against the price table. Fallbacks are logged at reload.

A price lookup that *misses entirely* is different again: it means the request
reached a vendor that never declared the model (a provider pin, an empty model
string, an unmapped `X-Api-Resource-Id`). There is no rate to reason from, so the
call bills $0 and logs a warning — a routing signal, not a pricing gap.

## Auth adapters

Derived from the wire name prefix — the operator never picks it. This is the
**egress** scheme (how songguo presents the *vendor* key upstream).

| Adapter | Wires | Scheme |
|---|---|---|
| `openai-compatible` | `openai/*` | `Authorization: Bearer <key>` |
| `anthropic-compatible` | `anthropic/*` | `x-api-key: <key>` + `anthropic-version` header |
| `volc-speech` | `volc/*` | `x-api-key: <key>` |

**Ingress** (how the *client* presents its songguo key) is independent of the wire:
songguo reads it from `Authorization: Bearer <key>` **or** `X-Api-Key: <key>`
(Authorization wins if both are sent), so an `X-Api-Key`-native SDK — Anthropic,
ByteDance ASR/TTS — switches to songguo by changing only the endpoint. Both
credential headers are stripped before the vendor key is written upstream.

## Resolved decisions

1. **Bare `GET /v1/models` returns one provider's list.** Model-listing carries no model string, so the provider comes from `X-Songguo-Provider` (or the priority-ordered default). The `openai/models`/`anthropic/models` suffix tie-break is resolved by the service's enabled wires (a service holds at most one `/models` wire per family). The response is that single provider's list, forwarded verbatim — Songguo never aggregates lists across providers (a merged list would be a synthesized response = transform).
2. **Volcengine paths are the native `/api/v3/...`** with no Songguo-local prefix; the provider comes from `X-Songguo-Provider` / the default (speech is model-less). Suffix matching is pinned by `wire/volc_test.go`.

## Implementation status

- **Full per-wire endpoints — done.** Provider config stores an explicit full upstream URL per wire (DB column `provider_endpoints.endpoint`), used as-is — no base+suffix join. `{model}` in the path is substituted with the request's model, and an endpoint query (e.g. Azure's `?api-version=…`) is merged with any inbound query, so non-uniform vendors like **Azure OpenAI** (`/openai/deployments/{model}/chat/completions?api-version=…`) work. Model-less / WebSocket forwarding uses the vendor's `origin` (scheme://host) with the inbound native path. Runtime vendors group by `(origin, adapter)`. There is no provider-level base URL or adapter concept. An idempotent startup migration folds pre-wire, wire-era, and old `provider_endpoints.base_url` schemas into canonical endpoints, then removes `provider_wires`, `providers.base_url`, and `providers.adapter`; fresh databases are created directly in the canonical shape.
- **Unified addressing — done.** One resolution path: match the wire by suffix, then select the provider `header → model → default`. `X-Songguo-Provider` (provider id) is a control header, stripped before forwarding. The default reuses provider priority — no separate flag. The `/x/<provider>/` passthrough is **removed**; the proxy is mounted at the native prefixes `/v1/` and `/api/v3/` (the latter is more specific than the admin `/api/`, so ServeMux routes it to the proxy). WebSocket upgrades carry the pin in the same header. `router.Candidates`/`CandidatesForProvider`/`AllCandidates` back the three selectors.
- **Still open:** `prd.md` §4.1 still models `Channel.base_url`; "Channel" (PRD) ≈ "provider"/"vendor" (config) should be reconciled when the PRD is next revised. A new native top-level path prefix (beyond `/v1/`, `/api/v3/`) would need an added proxy mount in `server.go`.
