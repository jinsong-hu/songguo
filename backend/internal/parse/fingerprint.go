package parse

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
)

// Fingerprint is the message-shape summary of a parsed request: how many
// messages it carried, a hash of the first, and a hash of all of them in order.
//
// It exists to answer one question about two captured requests WITHOUT reading
// either body: is the earlier one a redundant prefix of the later one? A coding
// agent re-sends its entire conversation on every turn, so within a stretch of
// growing context each request contains everything the ones before it did, and
// all but the last are duplicates. Head answers "do these start at the same
// place", Count answers "which reaches further", and Tail separates two
// same-length arrays that differ (a fork). Together they are the interval
// [start, end] of the conversation a request covers — with the start written as
// an identity rather than a number, because the only questions asked of it are
// comparisons, never "which message index is this".
//
// A zero Fingerprint means "unknown" and must never be read as "redundant"; see
// store.messageCover, which always keeps an unknown body rather than dropping
// it.
type Fingerprint struct {
	Count int    // number of request messages
	Head  []byte // hash of Input[0]; nil when there are no messages
	Tail  []byte // hash of every message in order; nil when there are no messages
}

// fingerprintBytes is how many bytes of the digest are kept. Truncated because
// these are compared, never inverted: at 16 bytes a session would need ~10^18
// messages before an accidental collision is likely, and the alternative to a
// collision here is one redundant body read, not a wrong answer.
const fingerprintBytes = 16

// Fingerprint summarizes the call's request messages.
//
// SHA-256 is not needed for its cryptographic properties — the payloads are
// trusted (songguo is key-gated and single-tenant) so there is no adversary
// crafting collisions, and a fast non-cryptographic hash would do. It is used
// because it is in the standard library, needs no dependency, and its cost is
// noise: hashing an already-materialized []Message runs in ~1-2ms for a 1.78 MB
// body against the ~10-20ms the caller already spent JSON-unmarshalling that
// same body. The expensive work is done by the time we are called.
func (c Call) Fingerprint() Fingerprint {
	if len(c.Input) == 0 {
		return Fingerprint{}
	}

	head := sha256.New()
	writeMessage(head, c.Input[0])

	tail := sha256.New()
	for _, m := range c.Input {
		writeMessage(tail, m)
	}

	return Fingerprint{
		Count: len(c.Input),
		Head:  head.Sum(nil)[:fingerprintBytes],
		Tail:  tail.Sum(nil)[:fingerprintBytes],
	}
}

// writeMessage feeds one message to h.
//
// Every field is LENGTH-PREFIXED rather than delimited. Without that,
// ["ab", "c"] and ["a", "bc"] would hash identically, and two different
// conversations would look like the same one — which is the single way this
// fingerprint could cause a wrong answer rather than a wasted read.
//
// It hashes the NORMALIZED parse.Message fields, not the wire bytes, and that is
// deliberate: Anthropic's cache_control breakpoints migrate through the message
// array as a conversation grows, so hashing raw JSON would change Head on turns
// where nothing about the conversation changed, splitting every run and undoing
// the whole optimization. The normalization has already dropped them by here.
func writeMessage(h hash.Hash, m Message) {
	writeField(h, m.Role)
	writeField(h, m.Text)
	writeField(h, m.ToolCallID)
	writeField(h, m.Name)

	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(m.ToolCalls)))
	h.Write(n[:])
	for _, tc := range m.ToolCalls {
		writeField(h, tc.ID)
		writeField(h, tc.Name)
		writeField(h, tc.Arguments)
	}
}

func writeField(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	h.Write(n[:])
	h.Write([]byte(s))
}
