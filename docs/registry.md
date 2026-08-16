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
- **`anthropic/messages`** — the reference shape: `input_tokens` (already fresh) + `cache_read_input_tokens` + `cache_creation_input_tokens` mapped straight through as three disjoint fields (cache-create bills at the model's `cache_write` rate, else `input`); `output_tokens_details.thinking_tokens` → `thinking_tokens`. Streaming merges `message_start.message.usage` (input) with `message_delta.usage` (output).
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
provider charges**. Otherwise a newly released model silently bills $0 until
someone notices — an under-bill indistinguishable from a free call in the
ledger. The substitution:

- **copies a whole real cost**, never a per-axis maximum. `cache_read: 0` means
  "no discount, charge full input", so a field-wise max would pick the *largest
  discount* and undercut a real model on cache-heavy traffic.
- **never crosses providers.** A provider with no usable rate keeps its $0 and
  is warned about. Borrowing globally would make one provider's bill a function
  of unrelated config.
- **lends only token rates.** Which quantity a brand-new model bills on is not
  knowable — an unpriced row declares no axes — so a media rate is never
  invented for it. Unpriced implies absent from the catalog, and a model new
  enough to be missing is a chat model in practice; media wires have small,
  fixed model sets that are hand-maintained. A speech model with no rate keeps
  its $0 and is warned about.
- **triggers on provenance, not on the number.** Only a model nobody ever
  stated a rate for is eligible. A zero *someone published* is a real price
  meaning free (`glm-4.7-flash`), and an operator `price_override` of zero is a
  deliberate "don't bill this". This distinction is only visible in the
  provenance: an empty cost and a published all-zero cost are the same value.
- **skips implausibly scaled rates** (a token rate above $1000/1M is almost
  certainly a basis mistake), so one typo can't become a provider's ceiling.

Every price carries a `source` (`catalog`, `feed`, `override`, `stored`,
`unpriced`, or `fallback:<model>`), visible in `GET /api/pricing` and the vendor
view, so a borrowed rate is never mistaken for a published one.

A price lookup that *misses entirely* is different again: the request reached a
vendor that never declared the model. There is no rate to reason from, so the
call bills $0 and logs a warning — a routing signal, not a pricing gap.

### Cost is additive axes, not a rate plus a unit

A model's cost declares one rate per metered quantity, and a call's cost is the
**sum over the axes that model declares**:

| axis | basis | `wire.Normalized` field |
|---|---|---|
| `input` | per 1M tokens | `InputTokens` |
| `output` | per 1M tokens | `OutputTokens` |
| `cache_read` | per 1M tokens | `CachedInputTokens` |
| `cache_write` | per 1M tokens | `CacheCreationTokens` |
| `character` | per character | `Chars` |
| `second` | per second | `Seconds` |
| `image` | per image | `Images` |
| `call` | per request | `Calls` |

The first four are models.dev's own fields at models.dev's basis, so generated
entries stay byte-identical to upstream. The rest are songguo's, at the basis
vendors publish, and exist because models.dev has no field of any kind for
per-character or per-second billing across all 5,901 models it lists.

Summing rather than dispatching on a `unit` fixes a real gap: a model billed on
two quantities — an audio model charging tokens *and* seconds — could previously
state only one, and silently metered $0 for the other. It also makes a
mismatched fallback inert rather than wrong: a borrowed token rate against a
speech call multiplies token counts that call never reported, and contributes
nothing.

Cache writes bill at `cache_write` when published, else at `input`. The 1.25x
write premium used to be ignored because no rate was available; models.dev
supplies one.

#### Context tiers

Many frontier models charge more once a prompt crosses a size. `gpt-5.6-luna` is
0.2/1.2 up to 272k and 0.4/1.8 above it; 8 of the 38 generated models carry a
bracket, and 334 do upstream. Ignoring them under-bills long agent contexts by
the tier multiple — the same class of error as a stale price, pointed the other
way, and aimed squarely at the traffic songguo exists to route.

```json
"cost": {
  "input": 0.2, "output": 1.2, "cache_read": 0.02,
  "tiers": [
    { "input": 0.4, "output": 1.8, "cache_read": 0.04,
      "tier": { "type": "context", "size": 272000 } }
  ]
}
```

Four rules, each of which is a way to get this wrong:

- **The bracket prices the whole request, not the excess.** Crossing 272k is a
  cliff: every token in the request reprices, including the first one.
- **Only the highest crossed bracket applies.** Brackets do not compound; a
  model with brackets at 32k and 256k bills a 300k request entirely at the 256k
  rate.
- **The threshold is measured on the PROMPT.** Input, cache reads and cache
  writes are disjoint and together are the prompt, so a cache-heavy request
  crosses on their sum. Output never counts — a vendor brackets on what you
  sent, not on what it wrote back.
- **A bracket states only what changes.** An axis it omits keeps the base rate,
  because reading an omission as zero would re-derive it from the raised input
  rate and over-bill. Seven models upstream publish exactly this shape.

`tier.type` is `context` on all 349 tiers upstream. An unrecognized type is
ignored rather than guessed at, so a new kind of bracket bills at the base rate
until it is implemented.

### Half the catalog's prices are generated, and the files say which half

`internal/catalog` embeds two files with two owners:

- **`models.json` is generated.** `make catalog-sync` (`backend/cmd/catalogsync`)
  rewrites it from models.dev; do not hand-edit. 38 models today.
- **`catalog.json` is hand-maintained.** The routing topology — endpoints,
  adapters, quirks, the `custom` template — which models.dev has no notion of
  because everything there is SDK-shaped rather than URL-shaped. Plus the 29
  models it cannot supply. 

`Load` merges them and the hand-written entry wins, which doubles as the way to
pin a rate the generator would otherwise overwrite.

Hand-typing the prices had drifted badly: `gpt-5.6-luna` sat at 1.00/6.00
against a published 0.20/1.20, with `gpt-5.6-terra`, `grok-4.5` and three `qwen`
models wrong the same way. `make catalog-check` now fails when they drift again.

What stays hand-maintained, and why it is not a gap to close:

- **All of `volcengine-*` (25 models).** models.dev has no first-party
  Volcengine provider; Doubao appears only under resellers (NanoGPT lists
  `doubao-seed-2-0-pro-260215` at 0.782/3.876 against Volcengine's own
  0.44/2.22).
- **Alibaba's and Zhipu's embeddings, and `glm-5-turbo`** — simply not listed.
- **Everything billed per character, second or call** — no schema for it exists
  upstream.

The Chinese vendors resolve to models.dev's **international** lists
(`alibaba`, `moonshotai`, `minimax`), matching the rates songguo already
carried. models.dev also publishes `-cn` variants; `alibaba-cn` prices
`qwen-max` at 0.345/1.377 against the international 1.6/6.4, so the choice is a
4.6x difference and deliberately explicit in `modelsdev.providerFor`.

**The generator is a seed, not the update mechanism.** It makes a fresh checkout
and an air-gapped install correct on first boot; `internal/pricefeed` keeps a
running gateway correct.

### Prices refresh themselves

`internal/pricefeed` re-reads models.dev **on startup and every
`SONGGUO_PRICE_REFRESH`** (default 24h; `0` disables it). It stores the result,
reloads the config, and new calls meter at the new rate. A generator that only
runs when a human runs it is how the catalog rotted 5x on a frontier model
without anyone noticing.

Resolution order — the ordering is the whole design:

| | source | why it is where it is |
|---|---|---|
| 1 | the operator's row, when marked `price_override` | a rate someone typed wins outright |
| 2 | the price feed (`feed`) | current, so a stale seed self-corrects |
| 3 | the embedded catalog (`catalog`) | the offline floor and first-boot seed |
| 4 | the operator's row as-is, else unpriced | |

A rate hand-pinned in `catalog.json` gets **no rule of its own**, because it
cannot collide: `modelsdev.Generate` skips every model the hand-written file
defines and the feed is built from `Generate`, so a pinned model never reaches
the feed to be overtaken by it. That invariant is held by a test
(`TestGenerateNeverEmitsAPinnedModel`) rather than by an extra layer here.

An automatic rate change is safe here for one specific reason: **cost is
computed at call time and persisted to `calls.cost`**. A refresh can only affect
future calls; it can never rewrite a ledger row. That is what separates it from
an automatic retry — there is no past state to revise.

It is still not allowed to go quiet. Every rate that moves is logged, a move
beyond 2x is escalated to WARN, and the resolved price reports `source: feed` so
`GET /api/pricing` never presents a refreshed number as a published one. The 2x
threshold is a *reporting* rule and deliberately not a limit: the 5x correction
this exists to catch would have been suppressed by any rule that refused large
moves.

What a refresh will not do:

- overwrite a pin or an override (rows 1–2 above);
- quote a model or provider the catalog does not declare — it reuses
  `modelsdev.Generate`, so Volcengine and every per-character/second/call model
  are structurally out of reach;
- accept an implausibly scaled rate, so an upstream basis slip cannot land;
- zero anything on failure. An unreachable feed, or one that quotes nothing,
  keeps the last stored refresh — persisted precisely so a restart during an
  upstream outage does not silently revert to a months-old seed.

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
