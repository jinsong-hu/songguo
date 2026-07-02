# CLAUDE.md — songguo

AI instructions for the songguo gateway. Read this before changing proxy behavior.

## Core invariant: we forward, we never touch the bytes

songguo is a **transparent gateway**, not a translator. Between client and vendor it:

- terminates the client's TLS and opens its own connection to the vendor (two
  separate requests — the outbound one is built fresh, not relayed at the socket),
- **rewrites headers only** — swaps the credential, retargets the URL/host,
- **forwards the request and response body verbatim.** The bytes the client sent
  are the bytes the vendor receives; the bytes the vendor returns are the bytes
  the client receives.

Reading the body is allowed (route by `model`, meter usage, capture for the
ledger) — all **read-only sniffing**. Mutating the body is **not**. There is no
sanctioned body rewrite. If you're tempted to add one, don't: put the behavior
on the caller, or handle it in metering, or leave it alone.

**Byte-transparency is absolute and outranks every feature.** When a feature
would require touching the client's bytes, we drop the feature — not the
transparency. Metering, usage accuracy, convenience quirks: all expendable if
the alternative is rewriting the payload. Do not propose a body mutation "just
this once" or behind an opt-in flag; the answer is no. This is a settled
decision, not a tradeoff to re-litigate.

> History: an `inject_stream_usage` quirk once rewrote streamed chat bodies to
> add `stream_options.include_usage`. It was removed — songguo does not add it
> for the caller. Consequence: vendors that omit usage from SSE unless the
> client sets that option will stream metered-zero. That is the accepted price
> of never touching the bytes. If a caller wants stream usage, the caller sets
> the option.

## What "forward verbatim" costs (known, accepted)

- The request body is currently **buffered** in RAM (bounded by `MaxBodyBytes`,
  default 25 MiB) so the proxy can read `model` for routing and replay it across
  failover candidates. Buffering ≠ mutating — the buffered bytes are forwarded
  unchanged. Large base64 media on `openai/responses` / `anthropic/messages`, or
  uploads on `volc/asr-file` / `openai/images` edits, can hit the cap → 413.
- Streaming the request body (byte-for-byte relay, like the WebSocket path
  already does) is possible but not implemented for HTTP wires; it trades away
  failover and needs mid-stream-truncation handling. Not a priority — raise the
  cap / add a memory budget first if 413s or RAM become the real pain.

## Key docs

| File | Purpose |
|------|---------|
| `docs/registry.md` | Wire catalogue — the supported endpoints/protocols and usage extraction |
| `docs/prd.md` | Product requirements |
| `docs/admin-api.md` | Admin/config API |
| `README.md` | Build & run |

## Build / test

No Go on the local box — build and test on the Mac mini (`ssh macmini`).
