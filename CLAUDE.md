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

## The credential header: the one header we own, read from wherever the client puts it

songguo's promise is a **drop-in endpoint swap** — the client points its existing
SDK at songguo, changes the base URL, and changes *nothing else*. The credential is
the one piece we must rewrite on both ends, so we handle it on the client's terms,
not ours:

- **Ingress (reading the caller's songguo key).** We accept it from
  `Authorization: Bearer <key>` (OpenAI-style) **or** `X-Api-Key: <key>`
  (Anthropic SDKs, ByteDance/Volcengine ASR & TTS). Authorization wins if both are
  present. So a client that natively authenticates with `X-Api-Key` needs no header
  surgery — the endpoint swap is enough. (See `clientKey` in `proxy.go`.)
- **Egress (presenting the vendor key).** We swap in the vendor credential using the
  header that vendor's adapter expects — `Authorization: Bearer` for
  openai-compatible, `X-Api-Key` for anthropic-compatible and volc-speech (see
  `applyUpstreamAuth` / `buildWSHandshake`).
- **No leak across headers.** Both credential headers are stripped from the outbound
  request before the vendor key is written, so the client's songguo key never
  reaches the vendor regardless of which header carried it. Non-credential
  `X-Api-*` headers (volc resource id, request id) still pass through verbatim.

This is a **header rewrite only** — fully consistent with byte-transparency above.
The body is never touched; we just read the credential from, and write it to,
whichever header the two ends use.

## What "forward verbatim" costs (known, accepted)

- The request body is **buffered** in RAM (so the proxy can read `model` for
  routing) and forwarded verbatim — buffering ≠ mutating. **There is no size
  ceiling.** The buffer grows to the
  actual payload size; songguo is key-gated and single-tenant, so payloads are
  trusted. Consequence: memory = payload × concurrency, and a runaway
  authenticated client can OOM the box rather than get a clean 413 — accepted
  tradeoff. If that ever becomes a real problem, the fix is to **stream** the
  body (byte-for-byte relay, like the WebSocket path), NOT to re-add a cap.
- Streaming the request body (byte-for-byte relay, like the WebSocket path
  already does) is possible but not implemented for HTTP wires; it needs
  mid-stream-truncation handling. Not a priority — raise the
  cap / add a memory budget first if 413s or RAM become the real pain.

## Behavioral transparency: one attempt, we never invent retries

songguo forwards **exactly one attempt** per request and surfaces whatever the
vendor returns — success **or** failure (`429`, `5xx`, a transport error) —
**verbatim**. It does **not** auto-retry and does **not** fail a request over to
another vendor mid-call. A client that wants to retry retries itself; that is the
client's decision, not ours.

This is byte-transparency's sibling on the behavior axis: just as we never invent
new *bytes*, we never invent new *attempts*. Silently turning one client request
into two or three against different vendors is exactly the kind of hidden
behavior a transparent proxy must not have — it masks failures, and it can replay
a request that had a side effect. So we don't.

Choosing **which** vendor serves a request, when a model has several candidates,
is a routing decision — priority → weighted round-robin, and the proxy forwards
to the **first** one.

There is **no automatic health demotion today**: a failing vendor is **not**
brought down on its own, so it stays selected until an operator changes config.
That is deliberate — "auto bring-down a bad provider" is a future, server-side
feature we haven't built yet, and when we do it belongs in the router as a
cross-request decision (steer the *next* request), **never** as a per-call retry.
Do **not** re-add per-call failover to fake it in the meantime. This is a settled
decision, like byte-transparency; don't re-litigate it behind a flag.

> History: the HTTP wire path once walked the whole candidate list, retrying the
> same request against the next vendor on `429`/`5xx`/transport error, and the
> router auto-demoted a failing vendor into a ~30s health cooldown. Both were
> removed 2026-07-03 — per-call failover on the HTTP **and** WebSocket paths, and
> the automatic health cooldown. Routing is now priority → weighted-RR, one
> attempt, no auto bring-down.

## Interface transparency: the client just changes the endpoint

Pointing a client at songguo is a **one-line change**: swap the base URL (and use
a songguo user key). Nothing else about the client's request has to change — no
songguo-specific header is ever *required* to get a request routed. This is a
hard invariant, the interface-shaped sibling of byte- and behavior-transparency,
**not** a nice-to-have. If a change would force every caller of some endpoint to
add a header, a query param, or a body field just to reach a vendor, that change
is wrong — fix the routing instead.

Routing is therefore **endpoint-first on every path, HTTP and WebSocket alike**:
the request path (plus the body `model`, when there is a body) selects the
vendor. Because a WebSocket upgrade carries no body, it routes on the **endpoint
alone** — the dialed path resolves to a wire, which resolves to a vendor. It does
**not** require a pin.

`X-Songguo-Provider` is an **optional disambiguator**, never a toll gate. It only
does something when one endpoint is served by several providers and the caller
wants to force one; absent it, the path narrows to the matching wire(s) and the
router picks (priority → weighted-RR). An unmatched path is a `404`
(`wire_unmatched`) — the fix is a **wire mapping in config**, never a header the
client must send. The one asymmetry: an explicit pin is trusted enough to reach a
provider's origin-only vendor that declares no wire; an unpinned request never
blind-pipes to an arbitrary origin.

> History: the WebSocket path once *required* `X-Songguo-Provider` and returned
> `400 songguo_missing_provider` without it, on the reasoning that a bodyless
> upgrade "cannot be model-routed." That conflated *can't model-route* with
> *can't route*: endpoint-first routing needs no model. Removed 2026-07-04 — WS
> now routes by endpoint like HTTP, and the pin is optional everywhere.

## Key docs

| File | Purpose |
|------|---------|
| `docs/registry.md` | Wire catalogue — the supported endpoints/protocols and usage extraction |
| `docs/prd.md` | Product requirements |
| `docs/admin-api.md` | Admin/config API |
| `README.md` | Build & run |

## Build / test

No Go on the local box — build and test on the Mac mini (`ssh macmini`).

## Git workflow

- Work in a fresh git worktree for each task; do not edit the primary checkout directly.
- Before creating any worktree, update the primary checkout first: switch to `main`, pull `origin/main` with `--ff-only`, then create the worktree branch from the updated base.
- Commit and push only after the user explicitly says to proceed.
- Before pushing, fetch and rebase on `origin/main`; do not merge.
- Push the task branch directly to `main` as `<branch>:main`.
- After pushing from a worktree, update the primary checkout again: switch to `main`, pull `origin/main` with `--ff-only`, then prune stale refs.
- After the push lands and the primary checkout is synced, remove the worktree and delete the local branch.

```sh
# --- before creating a worktree ---
cd <primary-checkout>
git switch main
git pull --ff-only origin main
git worktree add ../<worktree-name> -b <branch> main

# --- work in the worktree ---
cd ../<worktree-name>

# --- before pushing, after the user's "go" ---
git fetch origin
git rebase origin/main
git push origin <branch>:main

# --- after pushing from the worktree ---
cd <primary-checkout>
git switch main
git pull --ff-only origin main
git fetch --prune origin

# --- after the push lands ---
git worktree remove ../<worktree-name>
git branch -d <branch>
```

## On the MacBook (the dev machine) — no worktree

The MacBook is the dev machine. Work **directly in the primary checkout** — do **not** create a worktree there. Multiple sessions may be editing the same checkout at the same time, so treat other sessions' edits as coexisting WIP, not as something to clean up.

- Do not reset, stash, checkout-over, or clean files you didn't change — another session may be mid-edit.
- When committing, **selectively stage your own changes** (`git add <file>` / `git add -p`); never `git add -A`.
- **Superset rule for shared files:** if a file you changed was *also* changed by another session, include it in your commit anyway — stage the file as it stands on disk (its current contents are the superset of both sessions' edits). Don't try to split out only your hunks from a shared file; commit the whole file.
- Everything else in the git workflow above still applies (rebase not merge, push only on "go", `--ff-only` pulls).

## Local changes

- The primary checkout may contain user WIP. Do not overwrite, reset, or clean it unless the user explicitly asks.
- Stage only files or hunks owned by the current task; never use `git add -A` when unrelated changes exist.
- Prefer `git pull --ff-only` so Git never creates accidental merge commits.
