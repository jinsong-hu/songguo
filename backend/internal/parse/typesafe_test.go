package parse

import "testing"

// The request/response pair is the one proxy/typesafe_test.go forwards, so the
// parser is tested against bytes a real call is known to carry.
const (
	systemOneReqBody = `{"state":"Help! My payouts have been failing for 3 days.",` +
		`"model":"jev-latest",` +
		`"questions":{"is_urgent":{"type":"noul","instructions":"Does this convey urgency?"}}}`
	systemOneRespBody = `{"model":"jev-1.13.0",` +
		`"answers":{"is_urgent":{"type":"noul","noul":0.95}},` +
		`"usage":{"input_tokens":296,"output_tokens":20}}`
)

func parseSystemOneBodies(t *testing.T, req, resp string) (Call, error) {
	t.Helper()
	return Parse(Input{
		Wire:     "typesafe/systemone",
		Adapter:  "openai-compatible",
		Modality: "decision",
		ReqBody:  []byte(req),
		RespBody: []byte(resp),
	})
}

func TestSystemOneRoundTrip(t *testing.T) {
	c, err := parseSystemOneBodies(t, systemOneReqBody, systemOneRespBody)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if c.Format != "typesafe-systemone" {
		t.Fatalf("format = %q", c.Format)
	}
	if c.Model != "jev-latest" {
		t.Errorf("model = %q, want the request's jev-latest", c.Model)
	}
	if c.SystemOne == nil {
		t.Fatal("SystemOne is nil")
	}
	so := c.SystemOne
	if so.State != "Help! My payouts have been failing for 3 days." {
		t.Errorf("state = %q", so.State)
	}
	if len(so.Questions) != 1 {
		t.Fatalf("questions = %d, want 1", len(so.Questions))
	}
	q := so.Questions[0]
	if q.Key != "is_urgent" || q.Type != "noul" || q.Instructions != "Does this convey urgency?" {
		t.Errorf("question = %+v", q)
	}
	if string(q.Raw) != `{"type":"noul","instructions":"Does this convey urgency?"}` {
		t.Errorf("question raw = %s", q.Raw)
	}
	if len(so.Answers) != 1 {
		t.Fatalf("answers = %d, want 1", len(so.Answers))
	}
	a := so.Answers[0]
	if a.Key != "is_urgent" || a.Type != "noul" {
		t.Errorf("answer = %+v", a)
	}
	// The value is read from the key the answer's own "type" names.
	if string(a.Value) != "0.95" {
		t.Errorf("answer value = %s, want 0.95", a.Value)
	}
	if string(a.Raw) != `{"type":"noul","noul":0.95}` {
		t.Errorf("answer raw = %s", a.Raw)
	}
	if c.Tokens.Input != 296 || c.Tokens.Output != 20 {
		t.Errorf("tokens = %+v, want 296/20", c.Tokens)
	}
	if c.Tokens.CachedInput != 0 || c.Tokens.CacheWrite != 0 || c.Tokens.Reasoning != 0 {
		t.Errorf("tokens = %+v, want zero on the axes TypeSafe does not publish", c.Tokens)
	}
}

// A value whose key does not match the declared type is left unread rather than
// guessed at; Raw still carries everything.
func TestSystemOneUnknownTypeKeepsRawOnly(t *testing.T) {
	c, _ := parseSystemOneBodies(t, systemOneReqBody,
		`{"answers":{"is_urgent":{"type":"verdict","confidence":0.4}}}`)
	a := c.SystemOne.Answers[0]
	if a.Type != "verdict" {
		t.Errorf("type = %q", a.Type)
	}
	if a.Value != nil {
		t.Errorf("value = %s, want nil when the type names no present key", a.Value)
	}
	if string(a.Raw) != `{"type":"verdict","confidence":0.4}` {
		t.Errorf("raw = %s", a.Raw)
	}
}

// Go map iteration is randomized. Without the sort the render page would
// reorder the same call's questions between two views of it.
func TestSystemOneIsSortedByKey(t *testing.T) {
	req := `{"state":"s","questions":{"zeta":{"type":"noul"},"alpha":{"type":"score"},"mid":{"type":"choice"}}}`
	resp := `{"answers":{"zeta":{"type":"noul","noul":0.1},"alpha":{"type":"score","score":3},"mid":{"type":"choice","choice":"b"}}}`
	want := []string{"alpha", "mid", "zeta"}
	for i := 0; i < 20; i++ {
		c, err := parseSystemOneBodies(t, req, resp)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		for j, key := range want {
			if c.SystemOne.Questions[j].Key != key {
				t.Fatalf("questions[%d] = %q, want %q", j, c.SystemOne.Questions[j].Key, key)
			}
			if c.SystemOne.Answers[j].Key != key {
				t.Fatalf("answers[%d] = %q, want %q", j, c.SystemOne.Answers[j].Key, key)
			}
		}
	}
}

// Every parser here is defensive: a malformed capture yields whatever could be
// recovered plus a non-fatal error, never a panic.
func TestSystemOneMalformedBodies(t *testing.T) {
	cases := []struct{ name, req, resp string }{
		{"empty", "", ""},
		{"html error page", systemOneReqBody, "<html><body>502 Bad Gateway</body></html>"},
		{"truncated response", systemOneReqBody, `{"answers":{"is_urgent":{"type":"nou`},
		{"truncated request", `{"state":"Help! My pay`, systemOneRespBody},
		{"answers not an object", systemOneReqBody, `{"answers":[1,2,3]}`},
		{"questions not an object", `{"questions":"none"}`, systemOneRespBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseSystemOneBodies(t, tc.req, tc.resp)
			if err == nil {
				t.Fatalf("err = nil, want a non-fatal parse error")
			}
			if c.Format != "typesafe-systemone" {
				t.Errorf("format = %q, want a usable Call even on failure", c.Format)
			}
			if c.SystemOne == nil {
				t.Error("SystemOne is nil; a partial parse must still be renderable")
			}
		})
	}
}

// The fingerprint stays zero, and that is load-bearing rather than an
// oversight: System One has no conversation, so the "earlier request is a
// prefix of the later one" rule messageCover applies does not hold. Two calls
// sharing a state but asking different questions would collide on any
// state-derived fingerprint and the earlier body — with its questions — would
// be dropped from the session view. Zero means "unknown", which messageCover
// never reads as "redundant".
func TestSystemOneFingerprintIsZero(t *testing.T) {
	c, err := parseSystemOneBodies(t, systemOneReqBody, systemOneRespBody)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	f := c.Fingerprint()
	if f.Count != 0 || f.Head != nil || f.Tail != nil {
		t.Errorf("fingerprint = %+v, want the zero value", f)
	}
}
