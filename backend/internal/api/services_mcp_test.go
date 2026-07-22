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

// TestServicesGenerateImage verifies generate_image builds the native request,
// forwards the caller's key and provider pin to the proxy, and turns an
// OpenAI-shaped image response into MCP image content.
func TestServicesGenerateImage(t *testing.T) {
	s := newTestStore(t)
	_, key, err := s.CreateUser(store.NewUser{Name: "agent"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	a := newAPI(Deps{Store: s})

	var gotMethod, gotPath, gotAuth, gotProvider string
	var gotBody []byte
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotProvider = r.Header.Get("X-Songguo-Provider")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		png := base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"`+png+`"}]}`)
	})

	cs := connectServices(t, a, key, stub)

	if names := toolNames(t, cs); !names["generate_image"] {
		t.Fatalf("generate_image missing from tools/list")
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "generate_image",
		Arguments: map[string]any{
			"prompt":   "a red fox",
			"model":    "gpt-image-1",
			"size":     "1024x1024",
			"provider": "openai-main",
		},
	})
	if err != nil {
		t.Fatalf("call generate_image: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool error: %s", contentText(res.Content))
	}

	// The native request reached the proxy intact.
	if gotMethod != http.MethodPost || gotPath != "/v1/images/generations" {
		t.Errorf("proxy got %s %s, want POST /v1/images/generations", gotMethod, gotPath)
	}
	if gotAuth != "Bearer "+key {
		t.Errorf("proxy Authorization = %q, want the caller's key", gotAuth)
	}
	if gotProvider != "openai-main" {
		t.Errorf("proxy X-Songguo-Provider = %q, want openai-main", gotProvider)
	}
	for _, want := range []string{`"model":"gpt-image-1"`, `"prompt":"a red fox"`, `"size":"1024x1024"`} {
		if !strings.Contains(string(gotBody), want) {
			t.Errorf("forwarded body %q missing %s", gotBody, want)
		}
	}

	// The vendor image came back as decoded image content.
	var img *mcp.ImageContent
	for _, c := range res.Content {
		if ic, ok := c.(*mcp.ImageContent); ok {
			img = ic
		}
	}
	if img == nil {
		t.Fatalf("no image content in result: %+v", res.Content)
	}
	if string(img.Data) != "PNGDATA" {
		t.Errorf("image data = %q, want decoded PNGDATA", img.Data)
	}
}

// TestServicesGenerateImageForwardsError verifies a non-2xx proxy response is
// surfaced verbatim as an MCP tool error so the agent can self-correct.
func TestServicesGenerateImageForwardsError(t *testing.T) {
	s := newTestStore(t)
	_, key, err := s.CreateUser(store.NewUser{Name: "agent"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	a := newAPI(Deps{Store: s})

	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"type":"songguo_budget","message":"over budget"}}`)
	})

	cs := connectServices(t, a, key, stub)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "generate_image",
		Arguments: map[string]any{"prompt": "x", "model": "gpt-image-1"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
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

	// No key.
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: code = %d, want 401", rec.Code)
	}

	// Unknown key.
	req = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer nope")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown key: code = %d, want 401", rec.Code)
	}
}
