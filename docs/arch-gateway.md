# Songguo — Gateway Architecture

> The forwarding path. This is the product's core and its first priority:
> **nothing here waits on insights.** Companion to `arch.md` (the overall split)
> and `arch-insights.md` (the async fork downstream of this doc). The settled
> transparency invariants live in `CLAUDE.md`; this doc consolidates them and
> adds the two-phase call lifecycle.

## The one job

Terminate the client's TLS, authenticate it against a songguo user key, pick a
vendor, open a fresh connection to that vendor, forward the request, stream the
response back. Swap the credential; touch nothing else. Record what happened.
Return.

Everything the gateway does is on the critical path of a real API call, so every
line is judged by one question: does it add latency or risk to forwarding? The
recording it does is deliberately minimal and strictly downstream of serving the
client — the client already has its bytes before the ledger is finalized.

## Settled invariants (see `CLAUDE.md` for the full history)

These are not re-litigated here; they are the ground the gateway stands on.

- **Byte-transparency is absolute.** The request body the client sent is the body
  the vendor receives; the response body the vendor returned is the body the
  client receives. We rewrite **headers only** (the credential). Reading the body
  (to route on `model`, to meter usage, to capture for `raw`) is allowed —
  mutating it is not, ever, behind no flag.
- **The credential is the one header we own.** Ingress: read the caller's songguo
  key from `Authorization: Bearer` or `X-Api-Key` (Authorization wins). Egress:
  present the vendor key in whatever header that vendor's adapter expects. Both
  credential headers are stripped outbound so the songguo key never leaks.
- **One attempt, no invented retries.** The gateway forwards exactly one attempt
  and surfaces the vendor's outcome — success or `429`/`5xx`/transport error —
  verbatim. No per-call retry, no mid-call failover. A client that wants to retry
  retries itself. Repeated failures do demote a vendor, but only for the *next*
  request (see below).
- **Endpoint-first routing.** The request path selects the wire; the wire plus
  (when present) the body `model` selects the vendor (health → sticky session → priority →
  weight, first pick forwarded). `X-Songguo-Provider` is an optional
  disambiguator, never a required header. An unmatched path is a `404`.

## Request flow

```
ServeHTTP
  │
  ├─ 1. auth            clientKey(r) → GetUserByKey        (401 on miss)
  ├─ 1b. WS upgrade?    → handleWebSocket (raw byte pipe, endpoint-routed)
  ├─ 2. buffer body     readBody — no size ceiling, forwarded verbatim
  ├─ 3. resolve route   match wire by path → select provider (pin ?? model ?? default)
  │
  ├─ ── mint call id (UUID) ──────────────────────────────────────────┐
  ├─ ── create-at-start: INSERT calls row, status = pending ──────────┤ gateway-owned
  │                                                                    │ two-phase write
  ├─ 4. budget check    (402 denial recorded + captured)              │
  ├─ 5. rate limit      (429 denial recorded + captured)              │
  ├─ 6. forward one attempt                                           │
  │       buildUpstreamRequest → outbound.Do(route) → stream response │
  │       sniff usage in flight (wire extractor, read-only)          │
  │                                                                    │
  ├─ ── update-at-end: UPDATE calls row (status, usage, cost, ...) ───┘
  │
  └─ hand finalized record to INSIGHTS (async fork — fire and forget)
```

## UUID minting

The call id is a UUID, minted **at request-start**, before the vendor is dialed.
Two reasons this must come first:

1. **The two-phase write needs a stable handle.** The gateway creates the row,
   then later updates *that same row*. It needs the id before it has a response.
   A DB autoincrement is only known after insert; a minted UUID is known up
   front.
2. **Every per-call child keys off it.** `raw`, and the insights-side parse and
   composition records, are all 1:1 with the call id. Minting once, early, gives
   every downstream write the same key.

The id is a string everywhere — the store PK, the API JSON `id` field, and the
frontend route param. There is no integer id and no `strconv.ParseInt` on the
call id path.

## The two-phase call write

A `calls` row is written twice, and **incomplete calls are recorded** — this is
the point of the split.

**Phase 1 — create-at-start.** As soon as the route is resolved (user known,
vendor/model as resolved as routing allows), insert a `calls` row:

- `id` = the minted UUID
- `ts_start` = now
- identity: `user_id`, `model`, `modality`, `vendor`, `session_id`, tags,
  attribution
- `status` = pending (a sentinel — no upstream response yet)
- `ts_end` = null

**Phase 2 — update-at-end.** When forwarding completes — a served response, a
gateway denial, an upstream transport failure, or a client abort — update the
same row:

- `status` = the HTTP status the **client** received — the provider's own code
  for a forwarded call, ours for a denial
- `err` = **whose** doing it was: `""` for anything forwarded to a provider,
  otherwise one of the slugs in `internal/calls`
- `usage`, `input_tokens`, `output_tokens`, `cached_tokens`, `cost`,
  `latency_ms`, `ttft_ms`, `generation_ms`, `stream`, `confidence`, `wire`
- `ts_end` = now

**Read `status` and `err` together, never `status` alone.** songguo mints
statuses of its own — a budget refusal is `402`, an unmatched wire `404`, a
routing miss `502` — so on the integer a denial we issued is indistinguishable
from the same code coming back from the provider, and a `429` we raised looks
exactly like one the provider raised. The pair is unambiguous, and
`calls.OutcomeOf(status, err)` is the single classifier over it. The outcome is
*derived* rather than stored, which is what makes it correct for the rows already
in the ledger instead of only for traffic that arrives from now on.

Two consequences worth stating, because both were previously recorded as
something they were not:

- A stream that dies mid-body keeps `status = 200` — the client really did
  receive a 200 header, and rewriting it would be its own lie — and records the
  break in `err`. Both fields stay true.
- A transport failure keeps the real error text (`connection refused`, `no such
  host`, `certificate has expired`), which the client was already being told.

A row that has phase 1 but never phase 2 has `status = pending`, `ts_end = null`
— it left a trace instead of a hole. There are two ways to get there and the API
separates them, because "running right now" and "will never finish" are
different facts:

- **in flight** — created by the process now serving the API.
- **never finished** (`abandoned: true`) — created before this process booted,
  so nothing alive owns it. Deliberately *not* called "crashed": a crash, a clean
  `SIGTERM` and a `docker stop` mid-call all leave an identical row, so naming
  one of them would invent a cause.

Neither can detect a row that leaked to pending while the process stayed up —
only an invented timeout could, and songguo does not invent those. That is why an
in-flight row shows elapsed time and renders no verdict.

> History: `status = 0` used to mean "transport failure / no response". Nothing
> has written it since transport failures started being recorded as `502` plus a
> `transport_error:` slug; it survives only on old rows, where `OutcomeOf` still
> decodes it. The dashboard's "Transport" bucket keyed on it and was therefore
> permanently empty, while real transport failures hid inside the 5xx count.

Gateway-originated denials (unmatched `404`, scope `403`, budget `402`, rate
`429`) and upstream build/transport failures (`502`) still produce a finalized
`calls` row — they are outcomes, and the ledger records outcomes. Where a
served or synthesized response exists and capture is on, the matching `raw` row
is written too.

### Ordering guarantee

The client is served **before** phase 2 touches the ledger, and phase 2 happens
before the insights fork. Metering, pricing, ledger finalization, and the
insights hand-off are all strictly after the client already has its bytes. A
slow or failing write never delays or corrupts the response.

## Outbound connection routes

Each provider stores either no `proxy_id` (explicit **Direct**) or the id of one
reusable HTTPS/SOCKS5 proxy. `configsvc` resolves that reference while building
the immutable snapshot, so the hot path receives a complete route and never
queries SQLite while forwarding.

`internal/outbound` owns every provider-facing connection:

- Direct uses a transport with no proxy callback, so process-level
  `HTTP_PROXY`, `HTTPS_PROXY`, and `ALL_PROXY` variables are ignored.
- HTTPS proxies use TLS to the proxy and CONNECT for tunneled destinations,
  with optional Basic authentication.
- SOCKS5 proxies support optional username/password authentication.
- HTTP forwarding, transparent WebSockets, browser WebSocket tests, and
  provider connectivity probes all use the same route.

There is no silent fallback to Direct when a configured proxy fails. The single
attempt fails as an upstream transport error, preserving the gateway's routing
and observability semantics.

## `raw` capture

`raw` holds the full request and response bodies plus redacted headers, 1:1 with
the call id. It is:

- **Gated by the `capture` toggle** (a single global app setting, read once per
  request so a mid-flight config reload can't change an in-flight call's
  behavior). Off by default.
- **Redacted** — `Authorization`, `X-Api-Key`, `Api-Key`, `Cookie` are stripped
  before storage; no captured trace persists a secret.
- **Byte-identical** to what crossed the wire. For streams, the bytes are tee'd
  to the client and to an in-memory buffer simultaneously and flushed per chunk,
  so capture never buffers the client's stream.
- **Short-lived** — pruned at 7 days, independently of and earlier than the
  90-day `calls` prune.

Capture is the one place the gateway reads the full response body it would
otherwise just stream through. That read is byte-transparent (a tee, not a
rewrite) and, for the non-streaming path, already necessary for usage
extraction.

## What the gateway does NOT do

- It does **not** compute session rollups, context composition, or protocol
  parses. Those are insights, off the hot path (`arch-insights.md`).
- It does **not** wait for, retry, or check the health of the insights fork. It
  hands off and moves on. If the hand-off channel is full or the worker is dead,
  the gateway logs and drops — the forward already succeeded.
- It does **not** prune. Retention is analysis-side housekeeping.
- It does **not** translate bodies, map models, add stream options, or invent
  retries. (See `CLAUDE.md`.)

## Cost of forwarding verbatim (known, accepted)

The request body is buffered in RAM so the proxy can read `model` for routing,
then forwarded verbatim — buffering is not mutating. There is no size ceiling;
songguo is key-gated and single-tenant, so payloads are trusted and memory =
payload × concurrency. If that ever bites, the fix is to **stream** the body
(byte-for-byte relay, like the WebSocket path), not to re-add a size cap.
