package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"net/http"
	"testing"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/store"
	"github.com/songguo/songguo/internal/wire"
)

// --- frame builders ---

// wsBuildFrame assembles one unmasked WebSocket frame (server->client). forceLen
// pins the length encoding (0 = minimal, 16 = 2-byte, 64 = 8-byte) so tests can
// exercise each length form.
func wsBuildFrame(opcode byte, fin bool, payload []byte, forceLen int) []byte {
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	out := []byte{b0}
	n := len(payload)
	switch {
	case forceLen == 64 || (forceLen == 0 && n > 0xffff):
		out = append(out, 127)
		var sz [8]byte
		binary.BigEndian.PutUint64(sz[:], uint64(n))
		out = append(out, sz[:]...)
	case forceLen == 16 || (forceLen == 0 && n > 125):
		out = append(out, 126)
		var sz [2]byte
		binary.BigEndian.PutUint16(sz[:], uint16(n))
		out = append(out, sz[:]...)
	default:
		out = append(out, byte(n))
	}
	return append(out, payload...)
}

func wsFrame(opcode byte, fin bool, payload []byte) []byte {
	return wsBuildFrame(opcode, fin, payload, 0)
}

// volcJSONFrame wraps a JSON payload in a (uncompressed) volc server frame.
func volcJSONFrame(json []byte) []byte {
	return volcFrame(0b1001, 0, volcSerJSON, json)
}

// volcAudioFrame wraps raw bytes in a volc audio (raw serialization) frame,
// which the reassembler must skip.
func volcAudioFrame(audio []byte) []byte {
	return volcFrame(0b1011, 0, volcSerRaw, audio)
}

// volcGzJSONFrame builds a volc server frame whose JSON payload is gzip-flagged,
// exercising parseVolcResponse's gunzip path.
func volcGzJSONFrame(json []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(json)
	_ = zw.Close()
	gz := buf.Bytes()
	out := []byte{(0x01 << 4) | 0x01, (0b1001 << 4) | 0, (volcSerJSON << 4) | 0x01, 0x00}
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len(gz)))
	out = append(out, sz[:]...)
	return append(out, gz...)
}

// --- reassembler ---

func TestWSReassembler(t *testing.T) {
	var got [][]byte
	re := &wsReassembler{onMessage: func(m []byte) {
		got = append(got, append([]byte(nil), m...))
	}}

	jf := volcJSONFrame([]byte(`{"a":1}`))

	re.write(wsFrame(wsOpBinary, true, jf))         // 1: single JSON message
	re.write(wsFrame(wsOpPing, true, []byte("hi"))) // control frame ignored
	half := len(jf) / 2                             // 2: fragmented JSON message
	re.write(wsFrame(wsOpBinary, false, jf[:half]))
	re.write(wsFrame(wsOpContinuation, true, jf[half:]))
	re.write(wsFrame(wsOpBinary, true, volcAudioFrame(bytes.Repeat([]byte{0}, 300)))) // audio skipped

	if len(got) != 2 {
		t.Fatalf("messages = %d, want 2", len(got))
	}
	for i, m := range got {
		p, _, em := parseVolcResponse(m)
		if em != "" || string(p) != `{"a":1}` {
			t.Errorf("message %d = %q (err %q), want {\"a\":1}", i, p, em)
		}
	}
	if re.bad {
		t.Error("reassembler desynced unexpectedly")
	}
}

func TestWSReassemblerLengthForms(t *testing.T) {
	for _, n := range []int{200, 70000} { // forces 16-bit then 64-bit length
		var got int
		re := &wsReassembler{onMessage: func([]byte) { got++ }}
		json := append([]byte(`{"x":"`), append(bytes.Repeat([]byte("a"), n), []byte(`"}`)...)...)
		re.write(wsFrame(wsOpBinary, true, volcJSONFrame(json)))
		if got != 1 {
			t.Errorf("n=%d messages=%d, want 1", n, got)
		}
	}
}

func TestWSReassemblerMaskedDesyncs(t *testing.T) {
	re := &wsReassembler{onMessage: func([]byte) { t.Error("masked frame must not yield a message") }}
	// Set the mask bit on byte 1; downstream frames must be unmasked.
	frame := wsFrame(wsOpBinary, true, volcJSONFrame([]byte(`{"a":1}`)))
	frame[1] |= 0x80
	re.write(frame)
	if !re.bad {
		t.Error("masked downstream frame should desync the reassembler")
	}
}

// --- meter ---

func TestWSMeterASRDuration(t *testing.T) {
	w, ok := wire.Get("volc/asr-stream-async")
	if !ok {
		t.Fatal("wire volc/asr-stream-async not registered")
	}
	m := newWSMeter(w.Extract, nil)
	m.feed(wsFrame(wsOpBinary, true, volcAudioFrame(bytes.Repeat([]byte{1}, 320)))) // skipped
	m.feed(wsFrame(wsOpBinary, true, volcJSONFrame([]byte(`{"audio_info":{"duration":12500}}`))))

	ext := m.finish()
	if ext.Confidence != calls.ConfidenceMeasured {
		t.Fatalf("confidence = %q, want measured", ext.Confidence)
	}
	if ext.Norm.Seconds != 12.5 {
		t.Errorf("seconds = %v, want 12.5", ext.Norm.Seconds)
	}
}

func TestWSMeterTTSCharsGzip(t *testing.T) {
	w, ok := wire.Get("volc/tts-unidirectional-stream")
	if !ok {
		t.Fatal("wire volc/tts-unidirectional-stream not registered")
	}
	m := newWSMeter(w.Extract, nil)
	m.feed(wsFrame(wsOpBinary, true, volcGzJSONFrame([]byte(`{"usage":{"text_words":42}}`))))

	ext := m.finish()
	if ext.Norm.Chars != 42 {
		t.Errorf("chars = %v, want 42", ext.Norm.Chars)
	}
}

func TestWSMeterNoUsageUnknown(t *testing.T) {
	w, _ := wire.Get("volc/asr-stream-async")
	m := newWSMeter(w.Extract, nil)
	m.feed(wsFrame(wsOpBinary, true, volcJSONFrame([]byte(`{"result":{"text":"hi"}}`)))) // no audio_info

	if ext := m.finish(); ext.Confidence != calls.ConfidenceUnknown {
		t.Errorf("confidence = %q, want unknown (no usage seen)", ext.Confidence)
	}
}

// --- integration: a metered volc speech session records real cost ---

func volcSpeechVendorYAML(baseURL string) string {
	return `
vendors:
  - name: volc
    origin: ` + baseURL + `
    adapter: volc-speech
    served_models: [seed-asr]
    priority: 1
    credential: {id: credV, api_key: vendor-secret}
    wires: [volc/asr-stream-async]
    endpoints:
      volc/asr-stream-async: ` + baseURL + `/sauc/bigmodel_async
    prices:
      seed-asr: { input: 0.001, unit: per_second }
`
}

func TestWebSocketVolcSpeechBilling(t *testing.T) {
	// 2000ms of audio → 2s → per_second @ 0.001 = 0.002.
	script := wsFrame(wsOpBinary, true, volcJSONFrame([]byte(`{"audio_info":{"duration":2000}}`)))
	up := &wsMockUpstream{script: script}
	mock := wsMockServer(t, up)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, volcSpeechVendorYAML(mock.URL)), st)

	conn, br := dialProxyWS(t, env.server.URL, "/api/v3/sauc/bigmodel_async?model=seed-asr", key, "credV")
	defer conn.Close()

	if code := readStatusLine(t, br); code != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", code)
	}
	readHeaders(t, br)
	// Drain the scripted downstream bytes so the meter has seen them before close.
	if _, err := io.ReadFull(br, make([]byte, len(script))); err != nil {
		t.Fatalf("read downstream script: %v", err)
	}
	conn.Close()

	rows := waitForRows(t, env, 1)
	r := rows[0]
	if r.Vendor != "volc" || r.Model != "seed-asr" {
		t.Fatalf("row = %+v, want vendor=volc model=seed-asr", r)
	}
	if r.Wire != "volc/asr-stream-async" {
		t.Errorf("wire = %q, want volc/asr-stream-async", r.Wire)
	}
	if !approxEqual(r.Cost, 0.002) {
		t.Errorf("cost = %v, want 0.002", r.Cost)
	}
	if r.Confidence != string(calls.ConfidenceMeasured) {
		t.Errorf("confidence = %q, want measured", r.Confidence)
	}
	if _, ok := r.Usage["usage"]; !ok {
		t.Errorf("usage missing nested raw usage: %+v", r.Usage)
	}
}

// A non-volc realtime session stays metered at $0 (no decoder eligible).
func TestWebSocketNonVolcStaysZeroCost(t *testing.T) {
	up := &wsMockUpstream{}
	mock := wsMockServer(t, up)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, wsVendorYAML(mock.URL, "rt", "credR", "vendor-rt-secret")), st)

	conn, br := dialProxyWS(t, env.server.URL, "/v1/images/generations?model=realtime-model", key, "credR")
	defer conn.Close()
	if code := readStatusLine(t, br); code != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", code)
	}
	readHeaders(t, br)
	conn.Close()

	rows := waitForRows(t, env, 1)
	if rows[0].Wire != "openai/images" {
		t.Errorf("wire = %q, want openai/images", rows[0].Wire)
	}
	if rows[0].Cost != 0 {
		t.Errorf("cost = %v, want 0 for ineligible WS session", rows[0].Cost)
	}
}
