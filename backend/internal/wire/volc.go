package wire

import (
	"encoding/json"

	"github.com/songguo/songguo/internal/calls"
)

func init() {
	// Speech synthesis (语音合成, doubao-seed-tts) ships in three protocol
	// shapes, one wire each — mirroring how the LLM family splits chat vs
	// responses. Only the HTTP form is metered here; the two WebSocket forms
	// ride the realtime passthrough (see proxy/websocket.go), which relays raw
	// frames and never touches a wire's Extract. They still register so the
	// catalog can declare each capability separately and the WS passthrough can
	// prefer the matching vendor by suffix; an HTTP request that ever resolved
	// to them carries no usage and meters as unknown (zero).

	// HTTP unidirectional: one request, audio streamed back as newline-delimited
	// JSON chunks. Billed per input character (usage.text_words) when the client
	// sets X-Control-Require-Usage-Tokens-Return.
	register(Wire{
		Name:       "volc/tts-unidirectional",
		Suffixes:   []string{"/tts/unidirectional"},
		Modality:   calls.ModalityTTS,
		Extract:    volcTTSExtract,
		NewScanner: newVolcTTSScanner,
	})
	// WebSocket unidirectional stream: client sends text, server streams audio.
	register(Wire{
		Name:     "volc/tts-unidirectional-stream",
		Suffixes: []string{"/tts/unidirectional-stream"},
		Modality: calls.ModalityTTS,
		Extract:  volcTTSExtract,
	})
	// WebSocket bidirectional: text and audio stream in both directions over one
	// long-lived session.
	register(Wire{
		Name:     "volc/tts-bidirectional",
		Suffixes: []string{"/tts/bidirection"},
		Modality: calls.ModalityTTS,
		Extract:  volcTTSExtract,
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

	// Speech recognition (语音识别, doubao-seed-asr) likewise splits into three
	// wires. Only the HTTP file-recognition form is metered here; the two
	// WebSocket streaming forms ride the realtime passthrough and meter as
	// realtime $0 (see the synthesis note above).

	// File recognition (录音文件识别): an async submit→poll pair. submit returns
	// only an ack; the transcript and billed audio duration arrive on a later
	// query poll, so one wire covers both suffixes and meters whichever body
	// carries audio_info.duration. Billing lands on the query call (per_second
	// on the audio length); the submit call meters zero.
	register(Wire{
		Name:     "volc/asr-file",
		Suffixes: []string{"/auc/bigmodel/submit", "/auc/bigmodel/query"},
		Modality: calls.ModalitySTT,
		Extract:  volcASRExtract,
	})
	// Streaming recognition (大模型流式语音识别, doubao-seed-asr-2.0), async:
	// incremental hypotheses stream back as audio is fed in.
	register(Wire{
		Name:     "volc/asr-stream-async",
		Suffixes: []string{"/sauc/bigmodel_async"},
		Modality: calls.ModalitySTT,
		Extract:  volcASRExtract,
	})
	// Streaming recognition, nostream: same WebSocket session, but only the
	// final transcript is returned (no intermediate hypotheses).
	register(Wire{
		Name:     "volc/asr-stream-nostream",
		Suffixes: []string{"/sauc/bigmodel_nostream"},
		Modality: calls.ModalitySTT,
		Extract:  volcASRExtract,
	})
}

// volcASRExtract meters a Volcengine bigmodel file-ASR response by the
// recognized audio length: audio_info.duration is milliseconds, mapped to
// Seconds for per_second pricing. The submit ack and streaming sauc frames
// carry no audio_info, so they extract as unknown (metered zero) — only the
// file-ASR query poll bills.
func volcASRExtract(body []byte, _ Quirks) Extraction {
	if len(body) == 0 {
		return Extraction{Confidence: calls.ConfidenceUnknown}
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return Extraction{Confidence: calls.ConfidenceUnknown}
	}
	// Duration sits at audio_info.duration; some payloads nest the whole result
	// (audio_info included) under a "result" object, so try both.
	durMs := numAt(m, "audio_info", "duration")
	if durMs == 0 {
		durMs = numAt(m, "result", "audio_info", "duration")
	}
	if durMs == 0 {
		return Extraction{Confidence: calls.ConfidenceUnknown}
	}
	return Extraction{
		Raw:        map[string]any{"duration_ms": durMs},
		Norm:       Normalized{Seconds: durMs / 1000},
		Confidence: calls.ConfidenceMeasured,
	}
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
