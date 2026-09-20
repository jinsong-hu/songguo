package wire

import "github.com/songguo/songguo/internal/calls"

func init() {
	register(Wire{
		Name:     "typesafe/systemone",
		Suffixes: []string{"/systemone"},
		// System One is not chat — it answers typed questions about a state,
		// with no message array and no generated text. It is filed under the
		// chat modality anyway because the vocabulary has no "decision" member
		// and the empty modality means "management wire, serves no models",
		// which would take the wire out of model routing entirely. Nothing
		// downstream mistakes it for chat: compose.Compose and compose.ToolTurn
		// dispatch on the wire NAME, and this one matches none of their cases.
		Modality: calls.ModalityChat,
		Extract:  systemOneExtract,
		// NewScanner stays nil: the System One API has no streaming mode.
	})
}

// systemOneExtract meters a System One response: a flat top-level "usage" with
// input_tokens and output_tokens, and nothing else. TypeSafe publishes no cache
// and no reasoning axis, so those stay zero rather than being guessed at.
//
// Deliberately not openAINormalize: that function would produce these same two
// numbers today, via its prompt_tokens→input_tokens fallback, but it is
// parameterized by the cache_tokens quirk and assumes a cache-INCLUSIVE input
// total. Neither holds here, so borrowing it would tie this wire's meter to
// quirk logic it does not participate in.
func systemOneExtract(body []byte, _ Quirks) Extraction {
	usage := topLevelUsage(body)
	if usage == nil {
		return Extraction{Confidence: calls.ConfidenceUnknown}
	}
	return Extraction{
		Raw: usage,
		Norm: Normalized{
			InputTokens:  numAt(usage, "input_tokens"),
			OutputTokens: numAt(usage, "output_tokens"),
		},
		Confidence: calls.ConfidenceMeasured,
	}
}
