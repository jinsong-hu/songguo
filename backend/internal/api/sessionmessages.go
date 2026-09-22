package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"sort"

	"github.com/songguo/songguo/internal/store"
	"github.com/songguo/songguo/internal/wire"
)

// sessionMessagesView is the compact prompt material needed by the session
// Messages panel. Values stay in their native wire shape so the frontend can
// render protocol-specific tool, image, and reasoning blocks without receiving
// full request/response traces.
//
// System and Tools collect the request's top-level fields plus the instruction
// material some clients carry inside the message array instead — see
// hoistInlinePrompt.
type sessionMessagesView struct {
	SessionID string            `json:"session_id"`
	Model     string            `json:"model"`
	System    []json.RawMessage `json:"system"`
	Tools     []json.RawMessage `json:"tools"`
	Messages  []json.RawMessage `json:"messages"`
	// Reply is the assistant turn decoded from the call's captured RESPONSE,
	// in the same item shape as Messages — what the caller received, kept apart
	// from what it sent so the two are never read as one list.
	//
	// Only the single-call path fills it. A session view merges requests, and
	// every request after the first already carries the previous reply inside
	// its own history; decoding responses there would show each turn twice.
	Reply []json.RawMessage `json:"reply"`
	// OmittedRequests counts the oldest covering request bodies left unread
	// because the newer ones already filled sessionMessagesBudget. Non-zero
	// means the view starts part-way through the session.
	OmittedRequests int `json:"omitted_requests"`
}

// sessionMessagesBudget bounds the stored request bytes one Messages view
// reads. Past it the oldest covering bodies are left unread and counted in
// OmittedRequests, which the panel reports — a view that starts part-way
// through a session is a stated limit; a view that takes the host down with it
// is not.
//
// The merge still holds each admitted body a small number of times over (the
// raw message values, and the encoded response), so the multiple on this number
// is single digits rather than the order of magnitude it was when every message
// also decoded into a generic tree — see canonicalPromptKey. 32 MB on the 8 GB
// host the gateway shares with several other services leaves room for the
// forwarding path this whole file is subordinate to.
const sessionMessagesBudget = 32 << 20

type capturedPromptBody struct {
	Model        string          `json:"model"`
	System       json.RawMessage `json:"system"`
	Instructions json.RawMessage `json:"instructions"`
	Tools        json.RawMessage `json:"tools"`
	Messages     json.RawMessage `json:"messages"`
	Input        json.RawMessage `json:"input"`
}

type promptItem struct {
	raw json.RawMessage
	key string
}

func (a *api) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	view, err := a.sessionMessagesData(r.Context(), r.PathValue("id"))
	if err != nil {
		if r.Context().Err() != nil {
			return // the viewer left; nobody to answer
		}
		a.writeDataErr(w, "get session messages", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *api) sessionMessagesData(ctx context.Context, id string) (sessionMessagesView, error) {
	release, err := a.admitBodyRead(ctx)
	if err != nil {
		return emptyPromptView(id), err
	}
	defer release()

	requests, omitted, err := a.store.SessionRequests(ctx, id, sessionMessagesBudget)
	if err != nil {
		return emptyPromptView(id), err
	}
	if omitted > 0 {
		a.logger.Info("session messages omitted the oldest bodies over budget",
			"session", id, "read", len(requests), "omitted", omitted, "budget_mb", sessionMessagesBudget>>20)
	}

	prompts := make([]capturedPromptBody, 0, len(requests))
	for _, request := range requests {
		if request.Wire == "anthropic/count_tokens" {
			continue
		}
		// The former frontend path ignored malformed captures as well. One bad
		// request must not hide the rest of a session's conversation.
		if prompt, ok := decodeCapturedPrompt(request.ReqBody, request.ReqHeaders); ok {
			prompts = append(prompts, prompt)
		}
	}
	view := mergePrompts(prompts)
	view.SessionID = id
	view.OmittedRequests = omitted
	return view, nil
}

// handleCallMessages is the single-request sibling of handleSessionMessages:
// the same system/tools/messages view, built from one call's captured request
// alone, so a request detail page reads the prompt it actually sent.
func (a *api) handleCallMessages(w http.ResponseWriter, r *http.Request) {
	view, err := a.callMessagesData(r.Context(), r.PathValue("id"))
	if err != nil {
		if r.Context().Err() != nil {
			return // the viewer left; nobody to answer
		}
		a.writeDataErr(w, "get call messages", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// callMessagesData returns the prompt view for one call, or a *apiError (404)
// when no payload was captured for it. A capture that is not a JSON prompt body
// reads as an empty view rather than an error, as it does for a session.
func (a *api) callMessagesData(ctx context.Context, id string) (sessionMessagesView, error) {
	release, err := a.admitBodyRead(ctx)
	if err != nil {
		return emptyPromptView(""), err
	}
	defer release()

	p, err := a.store.GetPayload(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return emptyPromptView(""), notFoundErr("trace not found")
		}
		return emptyPromptView(""), err
	}
	view := emptyPromptView("")
	// A request body we cannot parse says nothing about the response, so the
	// reply is decoded either way.
	if prompt, ok := decodeCapturedPrompt(p.ReqBody, p.ReqHeaders); ok {
		view = mergePrompts([]capturedPromptBody{prompt})
	}
	if reply := a.capturedReply(id, p); len(reply) > 0 {
		view.Reply = reply
	}
	return view, nil
}

// capturedReply decodes the assistant turn out of one call's captured response.
//
// Wire and Stream are read from the call row rather than sniffed off the body:
// both are recorded fact about what was actually served, and guessing the
// protocol from the bytes would be the display inventing a post-mortem the
// ledger already knows the answer to. A wire with no reply shape, a call with
// no capture, or a body the decoder cannot read all return nothing to show.
func (a *api) capturedReply(id string, p store.Payload) []json.RawMessage {
	e, err := a.store.GetCall(id)
	if err != nil {
		return nil
	}
	w, ok := wire.Get(e.Wire)
	if !ok || w.DecodeReply == nil {
		return nil
	}
	body := p.RespBody
	if decoded, ok := decodeTraceBody(body, headerValue(p.RespHeaders, "Content-Encoding")); ok {
		body = decoded
	}
	return w.DecodeReply(body, e.Stream)
}

func emptyPromptView(sessionID string) sessionMessagesView {
	return sessionMessagesView{
		SessionID: sessionID,
		System:    []json.RawMessage{},
		Tools:     []json.RawMessage{},
		Messages:  []json.RawMessage{},
		Reply:     []json.RawMessage{},
	}
}

// decodeCapturedPrompt reads the prompt fields out of a captured request body,
// undoing any Content-Encoding first.
func decodeCapturedPrompt(body []byte, headers map[string]string) (capturedPromptBody, bool) {
	if decoded, ok := decodeTraceBody(body, headerValue(headers, "Content-Encoding")); ok {
		body = decoded
	}
	var prompt capturedPromptBody
	if err := json.Unmarshal(body, &prompt); err != nil {
		return capturedPromptBody{}, false
	}
	return prompt, true
}

// mergePrompts folds requests, oldest first, into one de-duplicated view.
func mergePrompts(prompts []capturedPromptBody) sessionMessagesView {
	view := emptyPromptView("")

	// adoptInlineSystem is decided once for the whole session rather than per
	// request: a session that hoisted an inline preamble out of some requests but
	// not others would hand mergePromptItems two different message shapes for the
	// same conversation, and the overlap merge would append instead of merge.
	adoptInlineSystem := true
	for _, prompt := range prompts {
		if hasJSONValue(topLevelSystem(prompt)) {
			adoptInlineSystem = false
			break
		}
	}

	// A message key exists for exactly one purpose: mergePromptItems comparing
	// one request's messages against another's. With a single request there is
	// nothing to compare it to — the overlap is zero by construction, the loop
	// never runs, and every key computed would be read by nobody. That is the
	// whole single-call path (callMessagesData passes one prompt), which is also
	// the path a request detail page takes on every open.
	needKeys := len(prompts) > 1

	seenSystem := map[string]struct{}{}
	seenTools := map[string]struct{}{}
	var messages []promptItem
	for _, prompt := range prompts {
		if view.Model == "" && prompt.Model != "" {
			view.Model = prompt.Model
		}

		view.System = appendUniquePromptValue(view.System, seenSystem, topLevelSystem(prompt))

		if tools, ok := rawJSONArray(prompt.Tools); ok {
			for _, tool := range tools {
				view.Tools = appendUniquePromptValue(view.Tools, seenTools, tool)
			}
		}

		items := requestMessageItems(prompt.Messages, prompt.Input, needKeys)
		inlineSystem, inlineTools, next := hoistInlinePrompt(items, adoptInlineSystem)
		for _, tool := range inlineTools {
			view.Tools = appendUniquePromptValue(view.Tools, seenTools, tool)
		}
		for _, block := range inlineSystem {
			view.System = appendUniquePromptValue(view.System, seenSystem, block)
		}
		messages = mergePromptItems(messages, next)
	}

	for _, item := range messages {
		view.Messages = append(view.Messages, item.raw)
	}
	return view
}

// topLevelSystem is the request's out-of-band instruction field: Anthropic's
// system, or the Responses API's instructions.
func topLevelSystem(prompt capturedPromptBody) json.RawMessage {
	if hasJSONValue(prompt.System) {
		return prompt.System
	}
	return prompt.Instructions
}

// inlinePromptItem is the sliver of a message item that decides whether it is
// conversation or instruction material the client folded into the array.
type inlinePromptItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Tools   json.RawMessage `json:"tools"`
	Content json.RawMessage `json:"content"`
}

// hoistInlinePrompt splits a request's message items into the instruction
// preamble some clients carry inline and the conversation proper, mirroring how
// internal/compose already weighs the same items (additional_tools as
// tool_schemas, system/developer roles as system). Without it a client that
// declares neither field at the top level — Codex's responses-lite shape, where
// the schemas ride in an additional_tools item and the prompt in a run of
// developer messages — renders an empty System and Tools panel while its
// Context tab reads correctly.
//
// Tool schemas are lifted wherever they appear: an additional_tools item is
// never conversation. The system run is lifted only when adoptSystem says the
// session declares no top-level system, and only from the head of the array.
// Both restrictions exist because a developer item is not always instructions —
// clients that do declare instructions out of band still send developer items
// for per-turn context and compaction summaries, and those are conversation.
func hoistInlinePrompt(items []promptItem, adoptSystem bool) (system, tools []json.RawMessage, rest []promptItem) {
	rest = make([]promptItem, 0, len(items))
	head := adoptSystem
	for _, item := range items {
		var decoded inlinePromptItem
		if err := json.Unmarshal(item.raw, &decoded); err != nil {
			// A bare string item, or anything else that is not an object: ordinary
			// conversation, and the end of any preamble.
			head = false
			rest = append(rest, item)
			continue
		}
		if decoded.Type == "additional_tools" {
			if inline, ok := rawJSONArray(decoded.Tools); ok {
				tools = append(tools, inline...)
			}
			continue
		}
		if head && (decoded.Role == "system" || decoded.Role == "developer") {
			value := decoded.Content
			if !hasJSONValue(value) {
				value = item.raw
			}
			system = append(system, value)
			continue
		}
		head = false
		rest = append(rest, item)
	}
	return system, tools, rest
}

// requestMessageItems splits one request's message array into items. withKeys
// asks for the comparison key each item carries; see mergePrompts for why a
// single-request view asks for none.
func requestMessageItems(messages, input json.RawMessage, withKeys bool) []promptItem {
	items, ok := rawJSONArray(messages)
	if !ok {
		items, ok = rawJSONArray(input)
	}
	if !ok && isJSONString(input) {
		items = []json.RawMessage{cloneRawMessage(input)}
	}
	if len(items) == 0 {
		return nil
	}
	out := make([]promptItem, 0, len(items))
	for _, raw := range items {
		item := promptItem{raw: cloneRawMessage(raw)}
		if withKeys {
			item.key = canonicalPromptKey(raw, true)
		}
		out = append(out, item)
	}
	return out
}

// mergePromptItems mirrors the old frontend overlap merge: a cumulative next
// request contributes only the tail after the existing-suffix/next-prefix
// overlap. When an agent compacts its context and no overlap exists, both the
// earlier history and the compacted continuation are retained.
func mergePromptItems(existing, next []promptItem) []promptItem {
	if len(next) == 0 {
		return existing
	}
	overlap := min(len(existing), len(next))
	for overlap > 0 {
		existingStart := len(existing) - overlap
		matches := true
		for i := 0; i < overlap; i++ {
			if existing[existingStart+i].key != next[i].key {
				matches = false
				break
			}
		}
		if matches {
			break
		}
		overlap--
	}
	return append(existing, next[overlap:]...)
}

func appendUniquePromptValue(dst []json.RawMessage, seen map[string]struct{}, raw json.RawMessage) []json.RawMessage {
	if !hasJSONValue(raw) {
		return dst
	}
	key := canonicalPromptKey(raw, false)
	if _, ok := seen[key]; ok {
		return dst
	}
	seen[key] = struct{}{}
	return append(dst, cloneRawMessage(raw))
}

func rawJSONArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var values []json.RawMessage
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return nil, false
	}
	if values == nil {
		values = []json.RawMessage{}
	}
	return values, true
}

func hasJSONValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func isJSONString(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 1 && trimmed[0] == '"' && json.Valid(trimmed)
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

// canonicalPromptKey produces a stable equality key for a captured JSON value:
// two values share a key exactly when they are the same value, independent of
// object key order — and, when stripCacheControl is set, of any cache_control
// members. Message comparisons strip it because the frontend does the same
// before rendering and comparing.
//
// It is a fixed-width DIGEST, and it is produced by walking the decoder's token
// stream rather than by unmarshalling into `any`. That is not a micro-
// optimization; it is what keeps the merge's cost proportional to the JSON
// instead of to a Go representation of it. Unmarshalling a captured prompt into
// `map[string]any` / `[]any` / boxed scalars costs an order of magnitude more
// heap than the bytes it came from, and the old implementation paid for that
// tree twice — once to decode, once more for the cache_control-stripped copy —
// before marshalling a canonical string it then RETAINED, one per message, for
// as long as the view was being built. Against the budget of session bodies
// upstream, that is the difference between a merge that costs megabytes and one
// that costs gigabytes on a host with no swap.
//
// The walk holds one sub-digest per member of each open object (objects must be
// sorted to be order-independent; arrays are written in order and need no
// buffer), so peak memory is the nesting depth times the widest object — tens
// of KB for a captured turn, whatever its size.
//
// The encoding is self-delimiting: every value writes a type tag, scalars write
// their length before their bytes, and arrays and objects write a terminator.
// Without that, adjacent siblings could run together and ["a","b"] would key the
// same as ["ab"].
func canonicalPromptKey(raw json.RawMessage, stripCacheControl bool) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	sum := sha256.New()
	if err := hashCanonicalValue(dec, sum, stripCacheControl); err != nil {
		// Not JSON we can walk. The old implementation fell back to the raw text
		// as the key; hash it instead, so one malformed capture cannot put a
		// body-sized string in the map. The domain tag keeps a malformed value
		// from ever colliding with a well-formed one.
		bad := sha256.Sum256(raw)
		return "r" + string(bad[:])
	}
	return "c" + string(sum.Sum(nil))
}

// hashCanonicalValue writes the next value in the stream to h.
func hashCanonicalValue(dec *json.Decoder, h hash.Hash, strip bool) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return hashCanonicalObject(dec, h, strip)
		case '[':
			return hashCanonicalArray(dec, h, strip)
		}
		return fmt.Errorf("canonical key: unexpected %q", t)
	case string:
		hashTagged(h, 's', []byte(t))
	case json.Number:
		// The literal as it was written, which is what json.Marshal of a
		// json.Number also emits — so 1 and 1.0 stayed distinct before and stay
		// distinct now.
		hashTagged(h, 'n', []byte(t.String()))
	case bool:
		if t {
			h.Write([]byte{'t'})
		} else {
			h.Write([]byte{'f'})
		}
	case nil:
		h.Write([]byte{'z'})
	default:
		return fmt.Errorf("canonical key: unexpected token %T", tok)
	}
	return nil
}

func hashCanonicalArray(dec *json.Decoder, h hash.Hash, strip bool) error {
	h.Write([]byte{'['})
	for dec.More() {
		if err := hashCanonicalValue(dec, h, strip); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // the closing ']'
		return err
	}
	h.Write([]byte{']'})
	return nil
}

func hashCanonicalObject(dec *json.Decoder, h hash.Hash, strip bool) error {
	type member struct {
		key    string
		digest []byte
	}
	var members []member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("canonical key: object key %T is not a string", tok)
		}
		if strip && key == "cache_control" {
			if err := skipJSONValue(dec); err != nil {
				return err
			}
			continue
		}
		sub := sha256.New()
		if err := hashCanonicalValue(dec, sub, strip); err != nil {
			return err
		}
		members = append(members, member{key: key, digest: sub.Sum(nil)})
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return err
	}

	// Sorting by key is what makes the digest independent of the order the
	// client serialized its object in. Ties keep their original order so that
	// the last of a duplicated key wins below, matching what unmarshalling into
	// a map would have done.
	sort.SliceStable(members, func(i, j int) bool { return members[i].key < members[j].key })

	h.Write([]byte{'{'})
	for i, m := range members {
		if i+1 < len(members) && members[i+1].key == m.key {
			continue // a later member repeats this key; that one is the value
		}
		hashTagged(h, 'k', []byte(m.key))
		h.Write(m.digest)
	}
	h.Write([]byte{'}'})
	return nil
}

// skipJSONValue consumes the next value without hashing it.
func skipJSONValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// hashTagged writes a tagged, length-prefixed scalar, so that neither the tag
// nor the bytes of one value can be read as part of the next.
func hashTagged(h hash.Hash, tag byte, b []byte) {
	var prefix [9]byte
	prefix[0] = tag
	binary.BigEndian.PutUint64(prefix[1:], uint64(len(b)))
	h.Write(prefix[:])
	h.Write(b)
}
