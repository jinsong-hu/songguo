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
is a routing decision — health → sticky session → priority → weight, and the proxy
forwards to the **first** one.

Two of those four deserve stating plainly, because they are easy to get backwards:

- **Priority is a strict tier, not a weight.** While any priority-1 vendor is
  live, priority-2 receives nothing. That keeps two knobs with one job each: same
  priority + different weights is *load balancing*, different priority is
  *failover*. To split 90/10, use one priority and weights 9 and 1.
- **Stickiness distributes sessions, not requests.** A session pins to whichever
  vendor served its last turn, so an agent conversation keeps one provider and
  its prompt cache — on a 200k-token context that is roughly a 10× difference in
  input cost, which makes it the most expensive routing decision songguo makes.
  Weight therefore decides where a *new* session lands, not where each request
  goes — and the weighted draw is taken **per credential**, so a provider that
  declares several protocols does not get several chances to win.
  A config reload clears health but **keeps** pins: a stale pin matches nothing
  and is overwritten on the next dispatch, whereas clearing pins would cost a
  cold prompt per active session on every operator edit. Health sorts **above** stickiness, so a pin is only ever consulted among
  vendors of equal health and can never hold a session on a broken one — the
  guarantee is structural. A client that sends no session header just gets the
  ordinary ordering; we never *require* a header (see interface transparency).

### Provider concurrency is enforced outside routing — it decides WHEN, not WHERE

`max_concurrency` bounds in-flight requests per **credential** (0 = unlimited).
A request to a full provider **waits** for a slot; it is never sent to a
different provider.

That is the whole reason it lives in `internal/concurrency` rather than in the
router's sort key. If capacity steered routing, a busy provider would push
sessions onto another vendor and throw away the prompt cache stickiness exists
to protect — a cold 200k-token context costs roughly ten times a warm one, far
more than the wait. So routing picks the provider and capacity decides when it
goes.

The queue is FIFO per credential and bounded **only by the caller's own
context**: songguo invents no timeout here, exactly as it invents no retries and
no refusals. A client unwilling to wait cancels, and cancelling frees the slot
at once (recorded as `499 songguo_client_gone`).

Per credential, not per vendor: the `(origin, adapter)` split gives one API key
several routing vendors, and the account quota belongs to the key.

Known cost: a queued request holds its buffered body in RAM, multiplying the
memory tradeoff already accepted for buffering bodies. `waiting` on
`GET /api/vendors` and the Providers page is the signal that a limit is set
below what the provider actually allows.

### Health demotion is cross-request, and that is the only shape allowed

A vendor that fails repeatedly **is** brought down automatically — but only for
the **next** request, never the current one. The router watches the outcome of
each dispatched attempt (`router.Report`) and ranks a failing vendor last. The
failing request itself is still surfaced verbatim; we do not retry it, and we do
not replay it anywhere else.

There are three health tiers, and only the middle one is time-based:

| tier | entered by | left by |
|---|---|---|
| **live** | default | — |
| **cooling** | `failThreshold` consecutive failures | the cooldown lapsing, or a success |
| **dead** | `deadAfter` consecutive *demotions* with no success between them | a success, or an operator |

Cooldowns **double** on each repeat demotion (30s → 1m → 2m → … → 15m), so a
vendor that is genuinely gone costs ~4 failed requests an hour instead of 120.

**Dead deliberately does not expire.** While any working vendor exists a dead one
is never chosen, so it never earns the success that would revive it — it waits
for a human, which is the right response to a provider that has failed this
consistently. What makes that safe is that dead still never *excludes*: if
everything is dead — a partition on our side rather than a real outage — the
least-bad candidate still gets the request and the first success clears it. So a
self-inflicted outage heals itself while a genuinely broken provider waits.

This is the *only* legal shape for auto bring-down, and the boundary is worth
stating plainly:

- **Allowed** — using past outcomes to pick a different `targets[0]` *before*
  dispatch. One client request still produces exactly one upstream attempt.
- **Forbidden** — noticing the failure of the in-flight request and trying
  somebody else. That invents an attempt, and no flag makes it acceptable.

Two invariants keep the first from sliding into the second:

1. **The router orders; it never denies and never filters.** `Select` returns a
   permutation of its candidates — health **demotes, never excludes**. A cooling
   vendor still serves if nothing healthier exists, so health state can never
   empty a candidate list or manufacture a gateway refusal.
2. **Detection is passive.** Health is learned from requests clients actually
   made. We never probe: inventing traffic to a credential the operator pays for
   is the sibling of inventing attempts and inventing bytes. The accepted price
   is real client failures — 3 to trip the first demotion, then 1 per cooldown
   window for as long as the vendor stays dead (the failure streak survives the
   cooldown, so a vendor that fails its first request back is re-demoted at
   once). Only a success clears the streak.

Not every failure counts, and the ones that do are not equal. The question a
signal answers is **"would an identical retry fail identically?"**

| | | |
|---|---|---|
| **neutral** | caller's fault | 400/404/408/422, client hung up mid-stream. Every vendor rejects these identically, so counting them would let one broken client walk the whole fleet out of rotation |
| **fail** | vendor's fault, ambiguous | timeouts, connection resets, unexpected EOF, temporary DNS failure, 5xx, 403. Real, but as plausibly about this request or the network as about the vendor. Needs corroboration — 3 in a row |
| **fail_model** | vendor is fine, this model is not | 429. Demotes the `(vendor, model)` pair for a fixed cooldown and leaves vendor health untouched — providers meter per model tier, so a hot `gpt-4o` must not walk `text-embedding-3` out of rotation |
| **fail_hard** | vendor's fault, conclusive | connection refused, DNS NXDOMAIN, unverifiable TLS certificate. Properties of the *endpoint*, not the request: one is enough to demote |
| **fail_credential** | credential's fault, conclusive | 401. Like fail_hard, but demotes **every vendor sharing that credential**, not just the one that observed it |

A transport failure carries **no status code** (the vendor never answered), so
`Classify` ignores status whenever the error is non-nil and inspects the error
value instead — `syscall.ECONNREFUSED`, `net.DNSError.IsNotFound`,
`tls.CertificateVerificationError`. This is why "timeout" and "connection
refused" get different weights despite both arriving as `status = 0`.

Two deliberate asymmetries:

- **401 is credential-scoped; everything else is endpoint-scoped.** A provider
  splits into several vendors by `(origin, adapter)`, and those hosts fail
  independently — so a dead host says nothing about its sibling. But they share
  one API key, and a revoked key is dead on all of them at once. Making each
  sibling rediscover that with its own failed client request would be re-proving
  a fact we already hold.
- **403 is not conclusive even though 401 is.** 403 can mean "this model is not
  on your plan", which is per-model rather than vendor-wide.

> History: the HTTP wire path once walked the whole candidate list, retrying the
> same request against the next vendor on `429`/`5xx`/transport error, and the
> router auto-demoted a failing vendor into a ~30s health cooldown. Both were
> removed 2026-07-03. Cross-request demotion came back 2026-07-27 — in the
> router, as designed — while per-call failover stayed dead. `TestNoPerCallFailover`
> and `TestHealthDemotionIsCrossRequest` are the paired regression guards: the
> first proves one request never becomes two attempts, the second proves the
> failure still moves the *next* request.

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
router picks (health → sticky → priority → weight). An unmatched path is a `404`
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

## Git workflow

- Work in a fresh git worktree for each task; do not edit the primary checkout directly.
- Before creating any worktree, update the primary checkout first: switch to `main`, fast-forward to `origin/main`, then create the worktree branch from the updated base.
- Commit and push only after the user explicitly says to proceed.
- Before pushing, fetch and rebase on `origin/main`; do not merge.
- Push the task branch directly to `main` as `<branch>:main`.
- After pushing from a worktree, update the primary checkout again: switch to `main`, fast-forward to `origin/main`, then prune stale refs.
- **Then remove the worktree — always, in the same step**, not "later". A worktree is scratch space for one task; leaving it behind means the next session inherits a stale checkout and an already-merged branch. Verify first: `git status --short` empty and `git log --oneline origin/main..HEAD` empty. If either is not, something is unpushed — stop and ask, never force.

```sh
# --- before creating a worktree ---
cd <primary-checkout>
git switch main
git fetch origin && git merge --ff-only origin/main
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
git fetch origin && git merge --ff-only origin/main
git fetch --prune origin

# --- then clean up: verify, then remove ---
git -C ../<worktree-name> status --short                    # must be empty
git -C ../<worktree-name> log --oneline origin/main..HEAD   # must be empty
git worktree remove ../<worktree-name>
git branch -d <branch>
```

`git fetch` + `git merge --ff-only origin/main` rather than `git pull --ff-only`:
with `pull.rebase=true` set, `pull` tries to rebase and refuses outright when the
checkout has WIP. Fetch-then-merge fast-forwards cleanly. Either form still
aborts when an incoming change touches a file you have uncommitted — that is
correct; commit or set aside the WIP rather than forcing past it.

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
