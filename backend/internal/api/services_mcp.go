package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The services MCP is the consumer-facing sibling of the admin MCP (mcp.go).
//
// Where the admin MCP (mounted at /admin/mcp, admin-key gated) exposes the
// control plane — spend, ledger, users, providers — the services MCP (mounted
// at the bare /mcp, consumer-key gated) exposes *capabilities* an agent invokes:
// image generation, and later speech. Each tool constructs a NATIVE vendor
// request and originates it THROUGH the transparent proxy in-process, so the
// call is routed, credential-swapped and metered exactly like any other proxy
// request — against the caller's own key.
//
// This does not bend byte-transparency. The proxy still forwards the bytes this
// layer hands it verbatim; the schema→native translation lives ABOVE the wire,
// in the tool, which is the only place a translation layer is allowed to be.
// The MCP tool is just an in-process proxy client that happens to speak MCP.

type servicesCtxKey int

// ctxConsumerKey holds the caller's raw songguo key, stashed by
// servicesAuthMiddleware and read synchronously in getServer (before the MCP
// session's context is detached), then closed over by each tool so the native
// call it originates carries the same key.
const ctxConsumerKey servicesCtxKey = 0

// NewServicesMCPHandler builds the consumer-facing services MCP server over
// stateless streamable HTTP, gated by a consumer key and dispatching each tool
// through proxy (the transparent gateway handler).
func NewServicesMCPHandler(d Deps, proxy http.Handler) http.Handler {
	a := newAPI(d)
	streamable := mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server {
			key, _ := r.Context().Value(ctxConsumerKey).(string)
			return a.buildServicesServer(key, proxy)
		},
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	return a.servicesAuthMiddleware(streamable)
}

// servicesAuthMiddleware authenticates the caller's consumer key the same way
// the proxy's ingress does — Authorization: Bearer <key> or X-Api-Key — and
// stashes it for getServer. Unknown/revoked keys get a clean 401 before the MCP
// layer runs. The key is validated here and re-forwarded to the proxy by each
// tool, so the proxy re-authenticates and meters against that user (this layer
// never trusts itself as the user).
func (a *api) servicesAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r.Header.Get("Authorization"))
		if key == "" {
			key = strings.TrimSpace(r.Header.Get("X-Api-Key"))
		}
		if key == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing consumer key")
			return
		}
		if a.store == nil {
			writeError(w, http.StatusInternalServerError, "internal", "no user store")
			return
		}
		if _, err := a.store.GetUserByKey(key); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid consumer key")
			return
		}
		ctx := context.WithValue(r.Context(), ctxConsumerKey, key)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// buildServicesServer registers the capability tool catalogue on a fresh server
// whose handlers close over the caller's key and the proxy.
func (a *api) buildServicesServer(key string, proxy http.Handler) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "songguo-services", Version: a.version}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "generate_image",
		Description: "Generate an image from a text prompt via an OpenAI-compatible image endpoint routed by Songguo. Returns the generated image(s). Args: prompt (required); model (required — the image model id to route to, e.g. the vendor's image model); size (optional, e.g. \"1024x1024\"); provider (optional — pin a specific configured provider id). Metered against your key.",
	}, a.svcGenerateImage(key, proxy))

	return srv
}

// --- generate_image ---

type generateImageArgs struct {
	Prompt   string `json:"prompt" jsonschema:"the text prompt to render"`
	Model    string `json:"model" jsonschema:"the image model id to route to"`
	Size     string `json:"size,omitempty" jsonschema:"image size, e.g. 1024x1024 (vendor default if omitted)"`
	Provider string `json:"provider,omitempty" jsonschema:"pin a specific configured provider id"`
}

type generateImageResult struct {
	Status int    `json:"status"`
	Model  string `json:"model,omitempty"`
	Images int    `json:"images"`
}

func (a *api) svcGenerateImage(key string, proxy http.Handler) func(context.Context, *mcp.CallToolRequest, generateImageArgs) (*mcp.CallToolResult, generateImageResult, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args generateImageArgs) (*mcp.CallToolResult, generateImageResult, error) {
		if strings.TrimSpace(args.Prompt) == "" || strings.TrimSpace(args.Model) == "" {
			return toolError("prompt and model are required"), generateImageResult{}, nil
		}
		body := map[string]any{"model": args.Model, "prompt": args.Prompt}
		if args.Size != "" {
			body["size"] = args.Size
		}
		status, resp := dispatchProxy(ctx, proxy, key, http.MethodPost, "/v1/images/generations", args.Provider, body)
		if status < 200 || status >= 300 {
			// Surface the vendor/gateway error verbatim so the agent can self-correct.
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: string(resp)}},
			}, generateImageResult{Status: status}, nil
		}
		content, n := imageContentFromOpenAI(resp)
		return &mcp.CallToolResult{Content: content}, generateImageResult{
			Status: status,
			Model:  args.Model,
			Images: n,
		}, nil
	}
}

// --- shared helpers ---

// dispatchProxy originates a native vendor request in-process against the
// transparent proxy, carrying the caller's key (and an optional provider pin),
// and returns the proxy's status and response bytes. The proxy does the real
// work — auth, routing, credential-swap, metering, verbatim forward.
func dispatchProxy(ctx context.Context, proxy http.Handler, key, method, path, provider string, body any) (int, []byte) {
	buf, err := json.Marshal(body)
	if err != nil {
		return http.StatusInternalServerError, []byte(err.Error())
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(buf)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	if provider != "" {
		req.Header.Set("X-Songguo-Provider", provider)
	}
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// imageContentFromOpenAI turns an OpenAI-shaped images response
// ({"data":[{"b64_json":...}|{"url":...}]}) into MCP content: base64 payloads
// become ImageContent, URLs become text. An unrecognized shape is handed back
// as raw text so nothing the vendor returned is hidden.
func imageContentFromOpenAI(body []byte) ([]mcp.Content, int) {
	var parsed struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Data) == 0 {
		return []mcp.Content{&mcp.TextContent{Text: string(body)}}, 0
	}
	var out []mcp.Content
	for _, d := range parsed.Data {
		switch {
		case d.B64JSON != "":
			raw, err := base64.StdEncoding.DecodeString(d.B64JSON)
			if err != nil {
				continue
			}
			out = append(out, &mcp.ImageContent{Data: raw, MIMEType: "image/png"})
		case d.URL != "":
			out = append(out, &mcp.TextContent{Text: d.URL})
		}
	}
	if len(out) == 0 {
		return []mcp.Content{&mcp.TextContent{Text: string(body)}}, 0
	}
	return out, len(out)
}

// toolError builds an MCP tool-level error result (IsError, message as text) so
// the model sees the failure and can self-correct, per the SDK's guidance.
func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}
