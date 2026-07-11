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
  verbatim. No per-call retry, no mid-call failover, no automatic health
  demotion. A client that wants to retry retries itself.
- **Endpoint-first routing.** The request path selects the wire; the wire plus
  (when present) the body `model` selects the vendor (priority → weighted
  round-robin, first pick forwarded). `X-Songguo-Provider` is an optional
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
  │       buildUpstreamRequest → client.Do → stream/copy response    │
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

- `status` = final HTTP status (or 0 for a transport failure / no response)
- `err`, `usage`, `input_tokens`, `output_tokens`, `cached_tokens`, `cost`,
  `latency_ms`, `ttft_ms`, `generation_ms`, `stream`, `confidence`, `wire`
- `ts_end` = now

A row that has phase 1 but never phase 2 is a visible in-flight or interrupted
call: `status = pending`, `ts_end = null`. A crash, a hang, or an aborted stream
leaves a trace instead of a hole. The dashboard reads these as pending /
interrupted rather than pretending the call never happened.

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
