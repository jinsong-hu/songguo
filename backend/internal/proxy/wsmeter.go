package proxy

// Asynchronous WebSocket metering for Volcengine speech wires.
//
// The transparent WS relay (websocket.go) is a raw byte pipe that must never be
// blocked. To recover real usage from the streamed session WITHOUT slowing the
// relay, the downstream (server->client) bytes are tee'd into a wsMeter: the
// relay goroutine only does a cheap, non-blocking feed, while a dedicated
// goroutine reassembles RFC 6455 frames, unwraps Volcengine's binary speech
// protocol with the existing parseVolcResponse (wstest.go), and runs the
// matched wire's extractor (volcASRExtract / volcTTSExtract). Metering is
// strictly best-effort: any backpressure or protocol anomaly degrades to "no
// usage" (cost 0), never a corrupt guess that could over-bill.

import (
	"sync/atomic"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/wire"
)

// WebSocket frame opcodes (RFC 6455 §5.2).
const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

const (
	// wsMaxFrameBuffer caps unparsed bytes (and any single frame's payload) so a
	// runaway length can't grow memory without bound. Exceeding it desyncs the
	// reassembler, which turns metering off for the session.
	wsMaxFrameBuffer = 8 << 20
	// wsMaxJSONMessage caps an assembled JSON (control/result) message. Audio
	// frames are dropped before they accumulate, so this only bounds JSON.
	wsMaxJSONMessage = 1 << 20
	// wsMeterChanDepth bounds queued downstream slabs; a full channel means the
	// decoder fell behind, which desyncs rather than blocks the relay.
	wsMeterChanDepth = 256
)

// wsReassembler reassembles RFC 6455 frames from the unmasked server->client
// byte stream and hands each complete *binary* message to onMessage. Server
// frames must be unmasked (RFC 6455 §5.1), so there is no mask handling. On any
// anomaly — a masked downstream frame, an oversized frame/message, or an
// unparseable backlog — it sets bad and stops, so metering reports no usage
// instead of mis-decoding. Modeled on lineScanner (internal/wire/scanner.go):
// write never blocks or errors.
type wsReassembler struct {
	buf       []byte
	msg       []byte // payload of an in-progress fragmented binary message
	inMsg     bool   // assembling a fragmented binary message
	skip      bool   // current binary message is non-JSON (audio); drop it
	decided   bool   // skip decision made for the current message
	bad       bool
	onMessage func([]byte)
}

func (w *wsReassembler) write(p []byte) {
	if w.bad {
		return
	}
	w.buf = append(w.buf, p...)
	for !w.bad {
		if !w.parseFrame() {
			break
		}
	}
	// A backlog we can't drain (a partial frame larger than the cap) is a desync.
	if !w.bad && len(w.buf) > wsMaxFrameBuffer {
		w.fail()
	}
}

func (w *wsReassembler) fail() {
	w.bad = true
	w.buf = nil
	w.msg = nil
}

// parseFrame consumes one complete frame from buf, returning false when more
// bytes are needed (or on desync).
func (w *wsReassembler) parseFrame() bool {
	if len(w.buf) < 2 {
		return false
	}
	b0, b1 := w.buf[0], w.buf[1]
	fin := b0&0x80 != 0
	opcode := b0 & 0x0f
	masked := b1&0x80 != 0
	lenCode := int(b1 & 0x7f)

	off := 2
	var payloadLen int
	switch lenCode {
	case 126:
		if len(w.buf) < off+2 {
			return false
		}
		payloadLen = int(w.buf[off])<<8 | int(w.buf[off+1])
		off += 2
	case 127:
		if len(w.buf) < off+8 {
			return false
		}
		var n uint64
		for i := 0; i < 8; i++ {
			n = n<<8 | uint64(w.buf[off+i])
		}
		off += 8
		if n > uint64(wsMaxFrameBuffer) {
			w.fail()
			return false
		}
		payloadLen = int(n)
	default:
		payloadLen = lenCode
	}

	// Downstream frames must be unmasked; a masked frame here is a protocol
	// anomaly we refuse to guess at.
	if masked {
		w.fail()
		return false
	}
	if len(w.buf) < off+payloadLen {
		return false
	}
	payload := w.buf[off : off+payloadLen]

	switch opcode {
	case wsOpBinary:
		w.beginMessage(payload, fin)
	case wsOpContinuation:
		w.continueMessage(payload, fin)
	case wsOpText, wsOpClose, wsOpPing, wsOpPong:
		// Not part of the volc binary protocol; ignore. Control frames may be
		// interleaved between data fragments, which is fine — w.msg is untouched.
	default:
		// Unknown opcode: skip the frame, keep parsing.
	}

	w.buf = w.buf[off+payloadLen:]
	return true
}

// beginMessage handles the first frame of a binary message. A single-frame
// message (fin) is dispatched immediately; a fragmented one starts accumulation
// (copied, since buf is reused as it advances).
func (w *wsReassembler) beginMessage(payload []byte, fin bool) {
	if fin {
		if isVolcJSON(payload) {
			w.onMessage(payload)
		}
		return
	}
	w.inMsg = true
	w.decided = false
	w.skip = false
	w.msg = append(w.msg[:0], payload...)
	w.decide()
}

func (w *wsReassembler) continueMessage(payload []byte, fin bool) {
	if !w.inMsg {
		return // stray continuation
	}
	if !w.skip {
		if len(w.msg)+len(payload) > wsMaxJSONMessage {
			w.skip, w.msg = true, w.msg[:0] // too big to be the JSON we want
		} else {
			w.msg = append(w.msg, payload...)
		}
	}
	if !w.decided {
		w.decide()
	}
	if fin {
		if !w.skip && len(w.msg) > 0 {
			w.onMessage(w.msg)
		}
		w.inMsg, w.decided, w.skip = false, false, false
		w.msg = w.msg[:0]
	}
}

// decide classifies the in-progress message as JSON-or-skip once the volc
// serialization nibble (byte 2 of the volc frame) is available.
func (w *wsReassembler) decide() {
	if w.decided || len(w.msg) < 3 {
		return
	}
	w.decided = true
	if !isVolcJSON(w.msg) {
		w.skip, w.msg = true, w.msg[:0]
	}
}

// isVolcJSON reports whether a volc frame's serialization nibble marks a JSON
// payload (vs raw audio). Layout: byte 2 is (serialization<<4)|compression.
func isVolcJSON(frame []byte) bool {
	return len(frame) >= 3 && (frame[2]>>4) == volcSerJSON
}

// wsMeter decodes downstream bytes off the relay's hot path. feed is called by
// the relay goroutine (non-blocking); a single run goroutine does the work; and
// finish drains and returns the metered extraction after the session closes.
type wsMeter struct {
	in       chan []byte
	done     chan struct{}
	extract  func([]byte, wire.Quirks) wire.Extraction
	quirks   wire.Quirks
	desynced atomic.Bool
	last     wire.Extraction // written by run, read after done closes
}

// newWSMeter starts the decoder goroutine. extract is the matched wire's
// extractor (volcASRExtract / volcTTSExtract).
func newWSMeter(extract func([]byte, wire.Quirks) wire.Extraction, quirks wire.Quirks) *wsMeter {
	m := &wsMeter{
		in:      make(chan []byte, wsMeterChanDepth),
		done:    make(chan struct{}),
		extract: extract,
		quirks:  quirks,
	}
	go m.run()
	return m
}

func (m *wsMeter) run() {
	defer close(m.done)
	re := &wsReassembler{onMessage: m.onMessage}
	for slab := range m.in {
		re.write(slab)
	}
	if re.bad {
		m.desynced.Store(true)
	}
}

// onMessage unwraps one assembled volc JSON frame and runs the extractor,
// keeping the latest non-zero usage (latest-wins, like the SSE scanners).
func (m *wsMeter) onMessage(frame []byte) {
	payload, _, errMsg := parseVolcResponse(frame)
	if errMsg != "" || len(payload) == 0 {
		return
	}
	ext := m.extract(payload, m.quirks)
	if normNonZero(ext.Norm) {
		m.last = ext
	}
}

// feed copies p (io.Copy reuses its buffer) and non-blockingly enqueues it. A
// full channel means the decoder fell behind: mark desynced and drop, so the
// relay is never back-pressured and metering reports no usage rather than a
// corrupt partial decode.
func (m *wsMeter) feed(p []byte) {
	if m.desynced.Load() {
		return
	}
	b := make([]byte, len(p))
	copy(b, p)
	select {
	case m.in <- b:
	default:
		m.desynced.Store(true)
	}
}

// finish closes the input, waits for the decoder to drain, and returns the
// metered extraction. A desynced session (or one with no usage frame) yields an
// unknown extraction (cost 0) — never a guess. Must be called once, after both
// relay directions have stopped (no concurrent feed).
func (m *wsMeter) finish() wire.Extraction {
	close(m.in)
	<-m.done
	if m.desynced.Load() || m.last.Confidence == "" {
		return wire.Extraction{Confidence: calls.ConfidenceUnknown}
	}
	return m.last
}

// wsMeterSink adapts a wsMeter to io.Writer so it can ride an io.MultiWriter on
// the downstream copy. Write never blocks or errors.
type wsMeterSink struct{ m *wsMeter }

func (s wsMeterSink) Write(p []byte) (int, error) {
	s.m.feed(p)
	return len(p), nil
}

// normNonZero reports whether any billable quantity was extracted.
func normNonZero(n wire.Normalized) bool {
	return n.InputTokens > 0 || n.OutputTokens > 0 || n.CachedInputTokens > 0 ||
		n.Calls > 0 || n.Images > 0 || n.Seconds > 0 || n.Chars > 0
}
