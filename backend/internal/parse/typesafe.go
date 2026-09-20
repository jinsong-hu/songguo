package parse

import (
	"encoding/json"
	"fmt"
	"sort"
)

// SystemOneQuestion is one entry of a System One request's questions map: the
// map key, the answer type asked for, and the instructions that define it.
type SystemOneQuestion struct {
	Key          string          `json:"key"`
	Type         string          `json:"type"` // noul | choice | score
	Instructions string          `json:"instructions,omitempty"`
	Raw          json.RawMessage `json:"raw,omitempty"` // the question object, verbatim
}

// SystemOneAnswer is one entry of a System One response's answers map. Value is
// read self-describingly: the answer object names its own type, and the value
// lives at the key of that name.
type SystemOneAnswer struct {
	Key   string          `json:"key"`
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value,omitempty"` // the value at the key named by Type
	Raw   json.RawMessage `json:"raw,omitempty"`   // the answer object, verbatim
}

// SystemOne is the parsed view of a System One call: the state the questions
// were asked about, the questions, and the answers.
type SystemOne struct {
	State     string              `json:"state,omitempty"`
	Questions []SystemOneQuestion `json:"questions,omitempty"` // sorted by key
	Answers   []SystemOneAnswer   `json:"answers,omitempty"`   // sorted by key
}

type systemOneReq struct {
	Model     string                     `json:"model"`
	State     string                     `json:"state"`
	Questions map[string]json.RawMessage `json:"questions"`
}

type systemOneResp struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
}

// parseSystemOne parses a TypeSafe System One call: a state plus a map of typed
// questions in, a map of typed answers out. There is no conversation, so Input
// and Output stay empty and the structure lands on Call.SystemOne.
//
// Two rules this parser follows deliberately:
//
// The typed value is read self-describingly, never guessed. An answer is
// {"type":"noul","noul":0.95} — the value lives at the key the "type" field
// names. When that key is absent, Value stays nil and Raw carries the object.
// Raw is kept verbatim on both sides so a field TypeSafe reports that we have
// not seen (a confidence, a rationale) still reaches the render page without
// this parser pretending to know its name.
//
// c.Input is left empty, so Call.Fingerprint stays zero — an omission with a
// reason. The fingerprint exists for exactly one job: letting
// store.messageCover skip a captured body a later request already contains, on
// the rule "same head + non-decreasing count ⇒ the later one is a superset".
// That assumes each request is a contiguous slice of one growing conversation.
// System One has no conversation: two calls sharing a state but asking
// different questions would collide on any state-derived fingerprint, and the
// earlier body would be dropped from the session view along with its questions.
// A zero fingerprint means "unknown", which messageCover guarantees is never
// read as "redundant"; the cost is one body read per System One call in a
// session, and the alternative is a wrong answer.
func parseSystemOne(in Input) (Call, error) {
	c := Call{Format: "typesafe-systemone"}
	so := &SystemOne{}

	var req systemOneReq
	reqErr := json.Unmarshal(in.ReqBody, &req)
	if reqErr == nil {
		c.Model = req.Model
		so.State = req.State
		for _, key := range sortedKeys(req.Questions) {
			raw := req.Questions[key]
			var q struct {
				Type         string `json:"type"`
				Instructions string `json:"instructions"`
			}
			_ = json.Unmarshal(raw, &q)
			so.Questions = append(so.Questions, SystemOneQuestion{
				Key:          key,
				Type:         q.Type,
				Instructions: q.Instructions,
				Raw:          cloneRaw(raw),
			})
		}
	}

	var resp systemOneResp
	respErr := json.Unmarshal(in.RespBody, &resp)
	if respErr == nil {
		if c.Model == "" {
			c.Model = resp.Model
		}
		for _, key := range sortedKeys(resp.Answers) {
			raw := resp.Answers[key]
			so.Answers = append(so.Answers, systemOneAnswer(key, raw))
		}
	}

	// Usage is a flat top-level object with input_tokens/output_tokens, the two
	// axes wire.systemOneExtract meters; cache and reasoning stay zero because
	// TypeSafe publishes neither.
	c.Tokens = topLevelTokens(in.RespBody)
	c.SystemOne = so
	c.finalize(in)

	switch {
	case len(in.RespBody) == 0:
		return c, fmt.Errorf("parse: empty response body")
	case respErr != nil:
		return c, fmt.Errorf("parse: system one response: %w", respErr)
	case reqErr != nil:
		return c, fmt.Errorf("parse: system one request: %w", reqErr)
	}
	return c, nil
}

// systemOneAnswer reads one answer object. The value is taken from the key the
// object's own "type" field names; anything else is left to Raw.
func systemOneAnswer(key string, raw json.RawMessage) SystemOneAnswer {
	a := SystemOneAnswer{Key: key, Raw: cloneRaw(raw)}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return a
	}
	_ = json.Unmarshal(fields["type"], &a.Type)
	if a.Type == "" {
		return a
	}
	if v, ok := fields[a.Type]; ok {
		a.Value = cloneRaw(v)
	}
	return a
}

// sortedKeys orders a map's keys. Go map iteration is randomized, so without
// this the questions and answers would reorder between two renders of the same
// call.
func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
