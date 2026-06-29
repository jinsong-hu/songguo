package proxy

// Browser-facing WebSocket test driver, mounted at /api/test/<wire-path>. Unlike
// the transparent /api/v3 proxy (which faithfully relays a real SDK client's
// handshake and byte stream), this endpoint adapts a *browser* to a vendor's
// streaming speech API: the browser opens a plain WebSocket, streams 16 kHz mono
// 16-bit PCM as binary messages, and receives the vendor's decoded JSON results
// back as text. The gateway opens a clean, native WebSocket to the vendor and
// speaks its binary frame protocol here, server-side, so the browser never deals
// with auth headers, gzip/sequence framing, or vendor handshake quirks.
//
// Credentials come in as query params (key/provider/resource) because a browser
// WebSocket can't set headers; this is a dashboard-only, same-origin test path.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/songguo/songguo/internal/router"
	"github.com/songguo/songguo/internal/store"
)

// newUUID returns a random RFC 4122 v4 UUID for the vendor's per-connection ids.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Volcengine streaming-speech binary protocol constants (see the vendor's
// reference client). A frame is: [0x11][type<<4|flags][ser<<4|comp][0x00]
// [uint32 payload size][payload].
const (
	volcMsgFullClient = 0b0001 // client config request
	volcMsgAudioOnly  = 0b0010 // client audio chunk
	volcMsgError      = 0b1111 // server error
	volcSerRaw        = 0b0000
	volcSerJSON       = 0b0001
	volcFlagLast      = 0b0010 // final audio chunk / last server package
)

type wsTestHandler struct {
	store  *store.Store
	router *router.Router
	logger *slog.Logger
}

// NewWSTestHandler builds the browser-facing streaming-ASR test endpoint.
func NewWSTestHandler(d Deps) http.Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &wsTestHandler{store: d.Store, router: d.Router, logger: logger}
}

func (h *wsTestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key := q.Get("key")
	providerID := q.Get("provider")
	resourceID := q.Get("resource")

	// The wire path is whatever follows /api/test.
	wirePath := strings.TrimPrefix(r.URL.Path, "/api/test")
	if !strings.HasPrefix(wirePath, "/") {
		wirePath = "/" + wirePath
	}

	// Auth: the key doubles as the consumer key (the dashboard's signed-in key).
	if _, err := h.store.GetUserByKey(key); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if providerID == "" {
		http.Error(w, "missing provider", http.StatusBadRequest)
		return
	}

	// Resolve the pinned provider to its upstream endpoint + credential, reusing
	// the proxy's wire matching and per-vendor path rewrite.
	targets, err := h.router.CandidatesForProvider(providerID)
	if err != nil || len(targets) == 0 {
		http.Error(w, "no upstream for provider", http.StatusBadGateway)
		return
	}
	matched, wireMap, _ := resolveWires(targets, http.MethodGet, wirePath)
	if len(matched) > 0 {
		targets = matched
	}
	t := targets[0]
	host, useTLS, requestTarget, terr := wsUpstreamTarget(t, wireMap[t.Vendor.Name], wirePath, "")
	if terr != nil {
		http.Error(w, "bad upstream endpoint", http.StatusBadGateway)
		return
	}
	scheme := "ws"
	if useTLS {
		scheme = "wss"
	}
	upstreamURL := scheme + "://" + host + requestTarget

	browser, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
		// The dashboard dev server (5173) connects cross-origin to the gateway
		// (8080); this endpoint is gated by the key query param, so allow it.
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return // Accept already wrote the response
	}
	defer browser.CloseNow()

	if err := h.driveASR(r.Context(), browser, upstreamURL, t.Credential.APIKey, resourceID); err != nil {
		h.logger.Warn("ws test asr session failed", "err", err, "vendor", t.Vendor.Name)
	}
}

// driveASR opens a clean native WS to the vendor, sends the config frame, then
// bridges browser PCM up (as audio frames) and vendor responses down (as JSON).
func (h *wsTestHandler) driveASR(ctx context.Context, browser *websocket.Conn, upstreamURL, apiKey, resourceID string) error {
	hdr := http.Header{}
	hdr.Set("X-Api-Key", apiKey)
	hdr.Set("X-Api-Resource-Id", resourceID)
	hdr.Set("X-Api-Request-Id", newUUID())
	hdr.Set("X-Api-Connect-Id", newUUID())
	hdr.Set("X-Api-Sequence", "-1")

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	up, _, err := websocket.Dial(dialCtx, upstreamURL, &websocket.DialOptions{
		HTTPHeader:      hdr,
		CompressionMode: websocket.CompressionDisabled,
	})
	cancel()
	if err != nil {
		browser.Close(websocket.StatusInternalError, "upstream connect failed")
		return fmt.Errorf("dial upstream: %w", err)
	}
	defer up.CloseNow()
	up.SetReadLimit(16 << 20)

	// Opening config frame.
	if err := up.Write(ctx, websocket.MessageBinary, volcConfigFrame(resourceID)); err != nil {
		browser.Close(websocket.StatusInternalError, "config send failed")
		return fmt.Errorf("send config: %w", err)
	}

	errc := make(chan error, 2)

	// Browser PCM -> vendor audio frames. A text "eof" (or the browser closing)
	// flushes a final empty audio frame so the vendor finalizes the transcript.
	go func() {
		for {
			typ, data, rerr := browser.Read(ctx)
			if rerr != nil {
				_ = up.Write(ctx, websocket.MessageBinary, volcFrame(volcMsgAudioOnly, volcFlagLast, volcSerRaw, nil))
				errc <- rerr
				return
			}
			if typ == websocket.MessageText {
				if strings.TrimSpace(string(data)) == "eof" {
					if werr := up.Write(ctx, websocket.MessageBinary, volcFrame(volcMsgAudioOnly, volcFlagLast, volcSerRaw, nil)); werr != nil {
						errc <- werr
						return
					}
				}
				continue
			}
			if werr := up.Write(ctx, websocket.MessageBinary, volcFrame(volcMsgAudioOnly, 0, volcSerRaw, data)); werr != nil {
				errc <- werr
				return
			}
		}
	}()

	// Vendor response frames -> browser JSON. Closes on the last package or error.
	go func() {
		for {
			typ, data, rerr := up.Read(ctx)
			if rerr != nil {
				errc <- rerr
				return
			}
			if typ != websocket.MessageBinary {
				continue
			}
			payload, last, errMsg := parseVolcResponse(data)
			if errMsg != "" {
				_ = browser.Write(ctx, websocket.MessageText, jsonBytes(map[string]any{"error": errMsg}))
				errc <- fmt.Errorf("upstream: %s", errMsg)
				return
			}
			if len(payload) > 0 {
				_ = browser.Write(ctx, websocket.MessageText, payload)
			}
			if last {
				errc <- nil
				return
			}
		}
	}()

	<-errc
	browser.Close(websocket.StatusNormalClosure, "")
	return nil
}

// volcConfigFrame builds the opening FULL_CLIENT_REQUEST: the JSON session config
// for 16 kHz mono PCM, uncompressed (the header declares no compression).
func volcConfigFrame(_ string) []byte {
	cfg := map[string]any{
		"user": map[string]any{"uid": "songguo-playground"},
		"audio": map[string]any{
			"format":  "pcm",
			"codec":   "raw",
			"rate":    16000,
			"bits":    16,
			"channel": 1,
		},
		"request": map[string]any{
			"model_name":      "bigmodel",
			"enable_itn":      true,
			"enable_punc":     true,
			"result_type":     "single",
			"show_utterances": true,
		},
	}
	payload, _ := json.Marshal(cfg)
	return volcFrame(volcMsgFullClient, 0, volcSerJSON, payload)
}

// volcFrame assembles one client frame (uncompressed): header + size + payload.
func volcFrame(msgType, flags, serialization byte, payload []byte) []byte {
	out := make([]byte, 0, 8+len(payload))
	out = append(out, (0x01<<4)|0x01) // protocol v1, header size 1 (4 bytes)
	out = append(out, (msgType<<4)|(flags&0x0f))
	out = append(out, (serialization<<4)|0x00) // no compression
	out = append(out, 0x00)                    // reserved
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len(payload)))
	out = append(out, sz[:]...)
	out = append(out, payload...)
	return out
}

// parseVolcResponse decodes a server frame, returning the JSON payload (gunzipped
// if flagged), whether it is the last package, and any error message.
func parseVolcResponse(b []byte) (payload []byte, last bool, errMsg string) {
	if len(b) < 4 {
		return nil, false, "short server frame"
	}
	msgType := b[1] >> 4
	flags := b[1] & 0x0f
	compression := b[2] & 0x0f
	last = flags&0x02 != 0
	off := 4
	if flags&0x01 != 0 { // sequence
		off += 4
	}
	if flags&0x04 != 0 { // event
		off += 4
	}
	if msgType == volcMsgError {
		if off+8 > len(b) {
			return nil, last, "malformed error frame"
		}
		code := binary.BigEndian.Uint32(b[off : off+4])
		off += 4
		size := binary.BigEndian.Uint32(b[off : off+4])
		off += 4
		end := clampEnd(off, int(size), len(b))
		msg := string(b[off:end])
		if msg == "" {
			msg = fmt.Sprintf("upstream error (code %d)", code)
		}
		return nil, last, msg
	}
	if off+4 > len(b) {
		return nil, last, ""
	}
	size := binary.BigEndian.Uint32(b[off : off+4])
	off += 4
	end := clampEnd(off, int(size), len(b))
	out := b[off:end]
	if compression == 0x01 {
		if dec, derr := gunzip(out); derr == nil {
			out = dec
		}
	}
	return out, last, ""
}

func clampEnd(off, size, n int) int {
	end := off + size
	if end > n {
		end = n
	}
	return end
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func jsonBytes(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
