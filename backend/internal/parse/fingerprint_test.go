package parse

import (
	"bytes"
	"testing"
)

// The fingerprint's one way to cause a WRONG answer rather than a wasted read is
// for two different conversations to hash alike. Field boundaries are
// length-prefixed precisely so concatenation cannot alias: without that,
// ["ab","c"] and ["a","bc"] collide and the cover would drop a distinct body as
// a duplicate.
func TestFingerprintIsNotAmbiguousAcrossFieldBoundaries(t *testing.T) {
	a := Call{Input: []Message{{Role: "user", Text: "ab"}, {Role: "user", Text: "c"}}}
	b := Call{Input: []Message{{Role: "user", Text: "a"}, {Role: "user", Text: "bc"}}}

	fa, fb := a.Fingerprint(), b.Fingerprint()
	if bytes.Equal(fa.Tail, fb.Tail) {
		t.Fatal("distinct message splits produced the same tail hash")
	}
	if bytes.Equal(fa.Head, fb.Head) {
		t.Fatal("distinct first messages produced the same head hash")
	}
}

// Head identifies where a conversation starts, so appending must not move it —
// that is what lets a growing run collapse to its last member.
func TestFingerprintHeadIsStableUnderAppend(t *testing.T) {
	base := []Message{{Role: "user", Text: "one"}, {Role: "assistant", Text: "two"}}
	grown := append(append([]Message{}, base...), Message{Role: "user", Text: "three"})

	f1 := Call{Input: base}.Fingerprint()
	f2 := Call{Input: grown}.Fingerprint()

	if !bytes.Equal(f1.Head, f2.Head) {
		t.Fatal("head changed on append; every turn would open a new run")
	}
	if f1.Count != 2 || f2.Count != 3 {
		t.Fatalf("counts = %d, %d; want 2, 3", f1.Count, f2.Count)
	}
	if bytes.Equal(f1.Tail, f2.Tail) {
		t.Fatal("tail did not change on append")
	}
}

// Compaction replaces the history with a summary, so the first message differs
// and the head must reflect that — this is the signal that opens a new run.
func TestFingerprintHeadChangesWhenHistoryIsReplaced(t *testing.T) {
	before := Call{Input: []Message{{Role: "user", Text: "original task"}, {Role: "assistant", Text: "work"}}}
	after := Call{Input: []Message{{Role: "user", Text: "summary of prior conversation"}, {Role: "assistant", Text: "work"}}}

	if bytes.Equal(before.Fingerprint().Head, after.Fingerprint().Head) {
		t.Fatal("head unchanged after the history was replaced")
	}
}

// Same length, one differing message: the fork case. Head and count both match,
// so tail is the only thing that separates them.
func TestFingerprintTailSeparatesSameLengthFork(t *testing.T) {
	a := Call{Input: []Message{{Role: "user", Text: "q"}, {Role: "assistant", Text: "answer A"}}}
	b := Call{Input: []Message{{Role: "user", Text: "q"}, {Role: "assistant", Text: "answer B"}}}

	fa, fb := a.Fingerprint(), b.Fingerprint()
	if !bytes.Equal(fa.Head, fb.Head) || fa.Count != fb.Count {
		t.Fatal("expected head and count to match for a fork")
	}
	if bytes.Equal(fa.Tail, fb.Tail) {
		t.Fatal("tail failed to separate a fork; the branch would be dropped")
	}
}

// Tool calls are part of a message's identity: two turns differing only in the
// tool they invoked are different turns.
func TestFingerprintCoversToolCalls(t *testing.T) {
	a := Call{Input: []Message{{Role: "assistant", ToolCalls: []ToolCall{{Name: "read", Arguments: `{"p":"a"}`}}}}}
	b := Call{Input: []Message{{Role: "assistant", ToolCalls: []ToolCall{{Name: "read", Arguments: `{"p":"b"}`}}}}}

	if bytes.Equal(a.Fingerprint().Tail, b.Fingerprint().Tail) {
		t.Fatal("tool call arguments did not affect the fingerprint")
	}
}

// A call with no request messages (embeddings, speech) has no shape to compare.
// The zero value is what the store persists as "unknown", and unknown is never
// treated as redundant.
func TestFingerprintOfEmptyInputIsZero(t *testing.T) {
	f := Call{}.Fingerprint()
	if f.Count != 0 || f.Head != nil || f.Tail != nil {
		t.Fatalf("empty input produced %+v, want zero", f)
	}
}
