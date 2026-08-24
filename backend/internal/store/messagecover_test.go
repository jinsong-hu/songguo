package store

import (
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

// shape builds a fingerprinted call. head/tail are given as short strings for
// readability; the cover only ever compares them.
func shape(id, agent string, tsSec int, count int64, head, tail string) callShape {
	return callShape{
		CallID:  id,
		AgentID: agent,
		TS:      time.Unix(int64(tsSec), 0),
		Count:   count,
		Head:    []byte(head),
		Tail:    []byte(tail),
		HasRaw:  true,
	}
}

func coverIDs(shapes []callShape) []string {
	out := []string{}
	for _, c := range messageCover(shapes) {
		out = append(out, c.CallID)
	}
	return out
}

func assertCover(t *testing.T, name string, shapes []callShape, want ...string) {
	t.Helper()
	got := coverIDs(shapes)
	if len(got) != len(want) {
		t.Fatalf("%s: cover = %v, want %v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: cover = %v, want %v", name, got, want)
		}
	}
}

// The base case: a conversation that only grows collapses to its last request,
// which contains every message the earlier ones did.
func TestCoverCollapsesGrowingRun(t *testing.T) {
	assertCover(t, "growing", []callShape{
		shape("r1", "", 1, 3, "A", "t3"),
		shape("r2", "", 2, 5, "A", "t5"),
		shape("r3", "", 3, 7, "A", "t7"),
	}, "r3")
}

// Compaction substitutes a summary for the history, so msg_head changes and a
// new run opens. Both runs' last members are needed: the messages replaced by
// the summary survive ONLY in the pre-compaction bodies.
func TestCoverKeepsBothSidesOfCompaction(t *testing.T) {
	assertCover(t, "compaction", []callShape{
		shape("r1", "", 1, 3, "A", "t3"),
		shape("r2", "", 2, 5, "A", "t5"),
		shape("r3", "", 3, 7, "A", "t7"),
		shape("r4", "", 4, 3, "S", "u3"), // summary replaces A..E
		shape("r5", "", 5, 4, "S", "u4"),
	}, "r3", "r5")
}

// Middle-drop truncation keeps the first message but discards the middle, so
// msg_head is unchanged and only the count drop reveals the boundary. This is
// the case a "group by head" rule would silently merge — losing the dropped
// middle, which lives only in the pre-truncation body.
func TestCoverSplitsOnCountDropWithSameHead(t *testing.T) {
	assertCover(t, "truncation", []callShape{
		shape("r1", "", 1, 400, "A", "t400"),
		shape("r2", "", 2, 500, "A", "t500"),
		shape("r3", "", 3, 52, "A", "u52"), // middle dropped, m1 kept
		shape("r4", "", 4, 60, "A", "u60"),
	}, "r2", "r4")
}

// A fork has the same head AND the same count, so only msg_tail separates it.
// Without the tail column the later body would be taken as a superset of the
// earlier and the other branch would vanish.
func TestCoverSplitsForkOnTail(t *testing.T) {
	assertCover(t, "fork", []callShape{
		shape("r1", "", 1, 3, "A", "abc"),
		shape("r2", "", 2, 3, "A", "abX"),
	}, "r1", "r2")
}

// A retry is the same request twice: same head, same count, same tail. It is a
// true duplicate and must collapse, which is exactly what distinguishes it from
// the fork above.
func TestCoverCollapsesIdenticalRetry(t *testing.T) {
	assertCover(t, "retry", []callShape{
		shape("r1", "", 1, 3, "A", "abc"),
		shape("r2", "", 2, 3, "A", "abc"),
	}, "r2")
}

// A subagent's context is an unrelated conversation, not a truncation of its
// parent's. Runs must not span agents or the subagent's shorter array would read
// as a boundary in the parent's run and vice versa.
func TestCoverNeverSpansAgents(t *testing.T) {
	assertCover(t, "agents", []callShape{
		shape("m1", "", 1, 3, "A", "t3"),
		shape("m2", "", 2, 5, "A", "t5"),
		shape("s1", "sub", 3, 2, "B", "v2"),
		shape("s2", "sub", 4, 4, "B", "v4"),
	}, "m2", "s2")
}

// An unknown fingerprint cannot be proven redundant, so it survives. This is the
// safety property the whole design rests on: missing information costs a
// redundant read, never a dropped message.
func TestCoverKeepsUnknownFingerprints(t *testing.T) {
	legacy := shape("r2", "", 2, 0, "", "")
	assertCover(t, "unknown", []callShape{
		shape("r1", "", 1, 3, "A", "t3"),
		legacy,
		shape("r3", "", 3, 7, "A", "t7"),
	}, "r1", "r2", "r3")
}

// raw is pruned at 7 days while calls lives 90, so a run's last member often has
// no body left. The cover falls back to the latest member that still has one:
// those are prefixes of the missing request, so they carry every message up to
// where they end. Failing the whole run instead would discard recoverable
// history.
func TestCoverFallsBackWhenLastCaptureIsPruned(t *testing.T) {
	pruned := shape("r3", "", 3, 7, "A", "t7")
	pruned.HasRaw = false
	assertCover(t, "pruned tail", []callShape{
		shape("r1", "", 1, 3, "A", "t3"),
		shape("r2", "", 2, 5, "A", "t5"),
		pruned,
	}, "r2")
}

// A run whose captures are all gone contributes nothing rather than erroring.
// The absence surfaces as messages that are not there, which is the honest
// outcome; inventing a placeholder would not be.
func TestCoverSkipsRunWithNoCaptures(t *testing.T) {
	a := shape("r1", "", 1, 3, "A", "t3")
	a.HasRaw = false
	b := shape("r2", "", 2, 5, "A", "t5")
	b.HasRaw = false
	assertCover(t, "all pruned", []callShape{a, b, shape("r3", "", 3, 3, "S", "u3")}, "r3")
}

// The cover is handed to a merge that reassembles oldest-first, so it must come
// back in time order even though runs are detected agent-major.
func TestCoverIsTimeOrdered(t *testing.T) {
	assertCover(t, "ordering", []callShape{
		shape("m1", "", 1, 3, "A", "t3"),
		shape("m2", "", 5, 5, "A", "t5"),
		shape("s1", "sub", 2, 2, "B", "v2"),
		shape("s2", "sub", 3, 4, "B", "v4"),
	}, "s2", "m2")
}

// seedSession appends a call with a captured body, optionally fingerprinted.
// count == 0 leaves the fingerprint unknown.
func seedSession(t *testing.T, s *Store, sessionID string, at time.Time, count int, head, tail, body string) string {
	t.Helper()
	id, err := s.AppendCall(calls.Entry{
		TS: at, SessionID: sessionID, Wire: "anthropic/messages", Status: 200,
	})
	if err != nil {
		t.Fatalf("AppendCall: %v", err)
	}
	if err := s.SavePayload(Payload{
		CallID: id, ReqBody: []byte(body), ReqContentType: "application/json",
		ReqHeaders: map[string]string{"User-Agent": "claude-cli/1.0"},
	}); err != nil {
		t.Fatalf("SavePayload: %v", err)
	}
	if count > 0 {
		if err := s.SaveMessageFingerprint(id, count, []byte(head), []byte(tail)); err != nil {
			t.Fatalf("SaveMessageFingerprint: %v", err)
		}
	}
	return id
}

// The end-to-end path (SQL → cover → body fetch) is covered by
// TestSessionRequestsReadsOnlyTheCoveringBodies in payloads_test.go, next to the
// unfingerprinted test whose fallback behavior it contrasts with. The two below
// cover what that one does not.

// A row the backfill could not fingerprint is marked with the -1 sentinel. It
// must still read as UNKNOWN to the cover — the mark changes only what the
// backfill retries, never what the reader trusts — so its body is still read.
func TestUnavailableFingerprintStillReadsAsUnknown(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

	unparseable := seedSession(t, s, "sess", base, 0, "", "", "opaque")
	if err := s.MarkFingerprintUnavailable(unparseable); err != nil {
		t.Fatalf("MarkFingerprintUnavailable: %v", err)
	}
	seedSession(t, s, "sess", base.Add(time.Minute), 5, "A", "t5", "turn2")

	got, err := s.SessionRequests("sess")
	if err != nil {
		t.Fatalf("SessionRequests: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("SessionRequests read %d bodies, want 2 (an unavailable fingerprint is not redundant)", len(got))
	}

	// ...but the backfill must not pick it up again, or it would re-read that
	// body on every run forever.
	pending, err := s.CallsNeedingFingerprint(10)
	if err != nil {
		t.Fatalf("CallsNeedingFingerprint: %v", err)
	}
	for _, p := range pending {
		if p.CallID == unparseable {
			t.Error("a call marked unavailable was offered to the backfill again")
		}
	}
}

// RequestHeaders must not load bodies, and must return the headers the client
// actually sent.
func TestRequestHeadersReturnsHeadersWithoutBodies(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	id := seedSession(t, s, "sess", base, 3, "A", "t3", "body-should-not-be-needed")

	got, err := s.RequestHeaders([]string{id, "missing-id"})
	if err != nil {
		t.Fatalf("RequestHeaders: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("RequestHeaders returned %d entries, want 1", len(got))
	}
	if got[id]["User-Agent"] != "claude-cli/1.0" {
		t.Errorf("headers = %v, want the captured User-Agent", got[id])
	}

	empty, err := s.RequestHeaders(nil)
	if err != nil {
		t.Fatalf("RequestHeaders(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("RequestHeaders(nil) = %v, want empty", empty)
	}
}

// The property the whole optimization claims: a long growing session reads one
// body, not N.
func TestCoverOnLongGrowingSessionReadsOneBody(t *testing.T) {
	var shapes []callShape
	for i := 1; i <= 284; i++ {
		shapes = append(shapes, shape(
			"r"+string(rune('0'+i%10)), "", i, int64(i), "A", "t"+time.Unix(int64(i), 0).String()))
	}
	if got := messageCover(shapes); len(got) != 1 {
		t.Fatalf("cover of a 284-turn growing session = %d bodies, want 1", len(got))
	}
}
