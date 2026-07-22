package api

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/songguo/songguo/internal/store"
)

// connectServices wires an in-memory MCP client to a services server built for
// key, dispatching through proxy. Mirrors connectMCP for the admin server.
func connectServices(t *testing.T, a *api, key string, proxy http.Handler) *mcp.ClientSession {
	t.Helper()
	srv := a.buildServicesServer(key, proxy)
	ctx := context.Background()

	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// newServicesFixture builds a services MCP client over a recording stub proxy,
// returning the client and the recorded request fields.
type recordedReq struct {
	method, path, auth, provider string
	headers                      http.Header
	body                         []byte
}

func servicesWithStub(t *testing.T, respStatus int, respBody string) (*mcp.ClientSession, *recordedReq) {
	t.Helper()
	s := newTestStore(t)
	_, key, err := s.CreateUser(store.NewUser{Name: "agent"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	a := newAPI(Deps{Store: s})
	got := &recordedReq{}
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.provider = r.Header.Get("X-Songguo-Provider")
		got.headers = r.Header.Clone()
		got.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(respStatus)
		_, _ = io.WriteString(w, respBody)
	})
	return connectServices(t, a, key, stub), got
}

func mustCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

// TestServicesTaskCatalogue verifies the task-taxonomy tools are all listed.
func TestServicesTaskCatalogue(t *testing.T) {
	cs, _ := servicesWithStub(t, 200, `{}`)
	names := toolNames(t, cs)
	for _, want := range []string{
		"text-to-image", "text-to-speech",
		"automatic-speech-recognition", "get-transcription",
		"text-to-video", "get-text-to-video",
	} {
		if !names[want] {
			t.Errorf("tool %q missing from tools/list", want)
		}
	}
}

// TestTextToImage verifies text-to-image builds the native request, forwards the
// caller's key and provider pin, and turns an OpenAI image response into image
// content.
func TestTextToImage(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
	cs, got := servicesWithStub(t, 200, `{"data":[{"b64_json":"`+png+`"}]}`)

	res := mustCall(t, cs, "text-to-image", map[string]any{
		"prompt": "a red fox", "model": "gpt-image-2", "size": "1024x1024", "provider": "openai-main",
	})
	if res.IsError {
		t.Fatalf("tool error: %s", contentText(res.Content))
	}
	if got.method != http.MethodPost || got.path != "/v1/images/generations" {
		t.Errorf("proxy got %s %s, want POST /v1/images/generations", got.method, got.path)
	}
	if !strings.HasPrefix(got.auth, "Bearer ") {
		t.Errorf("Authorization = %q, want a Bearer key", got.auth)
	}
	if got.provider != "openai-main" {
		t.Errorf("X-Songguo-Provider = %q, want openai-main", got.provider)
	}
	for _, want := range []string{`"model":"gpt-image-2"`, `"prompt":"a red fox"`, `"size":"1024x1024"`} {
		if !strings.Contains(string(got.body), want) {
			t.Errorf("body %q missing %s", got.body, want)
		}
	}
	var img *mcp.ImageContent
	for _, c := range res.Content {
		if ic, ok := c.(*mcp.ImageContent); ok {
			img = ic
		}
	}
	if img == nil || string(img.Data) != "PNGDATA" {
		t.Fatalf("want decoded image PNGDATA, got %+v", res.Content)
	}
}

// TestTextToSpeech verifies the TTS request headers/body and that the NDJSON
// audio chunks are reassembled into a single AudioContent.
func TestTextToSpeech(t *testing.T) {
	// Two NDJSON chunks: base64("AB") then base64("CD") -> "ABCD".
	line1 := `{"data":"` + base64.StdEncoding.EncodeToString([]byte("AB")) + `"}`
	line2 := `{"data":"` + base64.StdEncoding.EncodeToString([]byte("CD")) + `","usage":{"text_words":3}}`
	cs, got := servicesWithStub(t, 200, line1+"\n"+line2+"\n")

	res := mustCall(t, cs, "text-to-speech", map[string]any{
		"text": "你好", "voice": "zh_female_vv_uranus_bigtts", "model": "doubao-seed-tts",
	})
	if res.IsError {
		t.Fatalf("tool error: %s", contentText(res.Content))
	}
	if got.path != "/api/v3/tts/unidirectional" {
		t.Errorf("path = %s, want /api/v3/tts/unidirectional", got.path)
	}
	if got.headers.Get("X-Api-Resource-Id") != "doubao-seed-tts" {
		t.Errorf("X-Api-Resource-Id = %q, want the model", got.headers.Get("X-Api-Resource-Id"))
	}
	if got.headers.Get("X-Api-Request-Id") == "" {
		t.Errorf("missing X-Api-Request-Id")
	}
	if got.headers.Get("X-Control-Require-Usage-Tokens-Return") != "true" {
		t.Errorf("missing usage-return control header")
	}
	if !strings.Contains(string(got.body), `"speaker":"zh_female_vv_uranus_bigtts"`) {
		t.Errorf("body %q missing speaker", got.body)
	}
	var audio *mcp.AudioContent
	for _, c := range res.Content {
		if ac, ok := c.(*mcp.AudioContent); ok {
			audio = ac
		}
	}
	if audio == nil || string(audio.Data) != "ABCD" {
		t.Fatalf("want reassembled audio ABCD, got %+v", res.Content)
	}
}

// TestASRSubmitEchoesRequestID verifies the ASR submit sets the fixed resource
// id, generates an X-Api-Request-Id, and echoes it back for the poll.
func TestASRSubmitEchoesRequestID(t *testing.T) {
	cs, got := servicesWithStub(t, 200, `{"code":0,"message":"ok"}`)

	res := mustCall(t, cs, "automatic-speech-recognition", map[string]any{
		"audio_url": "https://example.com/a.wav", "provider": "volc-main",
	})
	if res.IsError {
		t.Fatalf("tool error: %s", contentText(res.Content))
	}
	if got.path != "/api/v3/auc/bigmodel/submit" {
		t.Errorf("path = %s, want submit", got.path)
	}
	if got.headers.Get("X-Api-Resource-Id") != "volc.seedasr.auc" {
		t.Errorf("resource id = %q, want volc.seedasr.auc", got.headers.Get("X-Api-Resource-Id"))
	}
	reqID := got.headers.Get("X-Api-Request-Id")
	if reqID == "" {
		t.Fatalf("submit did not set X-Api-Request-Id")
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("no structured content: %T", res.StructuredContent)
	}
	if sc["request_id"] != reqID {
		t.Errorf("structured request_id = %v, want the one sent (%s)", sc["request_id"], reqID)
	}
	if sc["provider"] != "volc-main" {
		t.Errorf("structured provider = %v, want volc-main", sc["provider"])
	}
}

// TestVideoPollForwardsPathAndProvider verifies get-text-to-video forwards the
// task id in the GET path and pins the provider.
func TestVideoPollForwardsPathAndProvider(t *testing.T) {
	cs, got := servicesWithStub(t, 200, `{"status":"running"}`)

	res := mustCall(t, cs, "get-text-to-video", map[string]any{
		"task_id": "task-123", "provider": "ark-main",
	})
	if res.IsError {
		t.Fatalf("tool error: %s", contentText(res.Content))
	}
	if got.method != http.MethodGet || got.path != "/api/v3/contents/generations/tasks/task-123" {
		t.Errorf("proxy got %s %s, want GET .../tasks/task-123", got.method, got.path)
	}
	if got.provider != "ark-main" {
		t.Errorf("provider = %q, want ark-main", got.provider)
	}
}

// TestServicesForwardsError verifies a non-2xx proxy response is surfaced
// verbatim as an MCP tool error so the agent can self-correct.
func TestServicesForwardsError(t *testing.T) {
	cs, _ := servicesWithStub(t, http.StatusPaymentRequired,
		`{"error":{"type":"songguo_budget","message":"over budget"}}`)

	res := mustCall(t, cs, "text-to-image", map[string]any{"prompt": "x", "model": "gpt-image-2"})
	if !res.IsError {
		t.Fatalf("expected tool error for 402 response")
	}
	if !strings.Contains(contentText(res.Content), "over budget") {
		t.Errorf("error text %q missing vendor message", contentText(res.Content))
	}
}

// TestServicesMCPRequiresConsumerKey verifies the HTTP-mounted services MCP is
// gated by a consumer key: missing or unknown keys get 401 before any tool runs.
func TestServicesMCPRequiresConsumerKey(t *testing.T) {
	s := newTestStore(t)
	stub := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h := NewServicesMCPHandler(Deps{Store: s}, stub)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: code = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer nope")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown key: code = %d, want 401", rec.Code)
	}
}
