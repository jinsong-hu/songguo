package wire

import (
	"encoding/json"

	"github.com/songguo/songguo/internal/calls"
)

func init() {
	register(Wire{
		Name:       "volc/tts",
		Suffixes:   []string{"/tts/unidirectional"},
		Modality:   calls.ModalityTTS,
		Extract:    volcTTSExtract,
		NewScanner: newVolcTTSScanner,
	})
	// Voice-cloning management: train a custom voice (/tts/voice_clone) and
	// poll its status (/tts/get_voice). These return no usage object — the
	// voice-slot fee is billed out-of-band on first synthesis — so they meter
	// as free, like a model-listing endpoint.
	register(Wire{
		Name:     "volc/voice-clone",
		Suffixes: []string{"/tts/voice_clone", "/tts/get_voice"},
		Modality: calls.ModalityTTS,
		Extract:  zeroCostExtract,
		ZeroCost: true,
	})
}

// volcTTSExtract meters a Volcengine speech-synthesis response. Billing is by
// input text characters (usage.text_words, punctuation included), returned
// when the client sets X-Control-Require-Usage-Tokens-Return; it maps to
// Chars for per_char pricing.
func volcTTSExtract(body []byte, _ Quirks) Extraction {
	return volcTTSNormalize(topLevelUsage(body))
}

func volcTTSNormalize(usage map[string]any) Extraction {
	if usage == nil {
		return Extraction{Confidence: calls.ConfidenceUnknown}
	}
	return Extraction{
		Raw:        usage,
		Norm:       Normalized{Chars: numAt(usage, "text_words")},
		Confidence: calls.ConfidenceMeasured,
	}
}

// volcTTSScanner meters the HTTP-chunked streaming form: newline-delimited
// JSON objects (bare, not SSE-framed), with usage carried on the chunks that
// report it. The latest non-null usage wins.
type volcTTSScanner struct {
	lineScanner
	usage map[string]any
}

func newVolcTTSScanner(_ Quirks) StreamScanner {
	s := &volcTTSScanner{}
	s.onLine = s.processLine
	return s
}

func (s *volcTTSScanner) processLine(line []byte) {
	var env struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return
	}
	if env.Usage != nil {
		s.usage = env.Usage
	}
}

func (s *volcTTSScanner) Result() Extraction {
	return volcTTSNormalize(s.usage)
}
