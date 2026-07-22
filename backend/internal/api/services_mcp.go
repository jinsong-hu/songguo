package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The services MCP is the consumer-facing sibling of the admin MCP (mcp.go).
//
// Where the admin MCP (/admin/mcp, admin-key gated) exposes the control plane,
// the services MCP (the bare /mcp, consumer-key gated) exposes songguo's
// services organized by task — the taxonomy the wires already declare via
// Modality: Text-to-Image, Text-to-Speech, Automatic Speech Recognition,
// Text-to-Video.
//
// Every tool is a thin typed front over EXACTLY ONE wire call. It builds the
// native vendor request, forwards it through the transparent proxy in-process
// (carrying the caller's key and an optional provider pin), and returns the
// vendor's bytes verbatim. It never waits, never retries, never mints its own
// task handle: an async submit->poll lifecycle is owned by the caller, exactly
// as when hitting the proxy directly. The schema->native translation lives in
// the tool, above the wire, so byte-transparency holds and each call meters
// independently.

type servicesCtxKey int

// ctxConsumerKey holds the caller's raw songguo key, stashed by
// servicesAuthMiddleware and read synchronously in getServer (before the MCP
// session context detaches), then closed over by each tool so the native call
// it originates carries the same key.
const ctxConsumerKey servicesCtxKey = 0

// NewServicesMCPHandler builds the consumer-facing services MCP over stateless
// streamable HTTP, gated by a consumer key and dispatching each tool through
// proxy (the transparent gateway handler).
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
// tool, so the proxy re-authenticates and meters against that user.
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

// buildServicesServer registers the task-taxonomy tool catalogue on a fresh
// server whose handlers close over the caller's key and the proxy.
func (a *api) buildServicesServer(key string, proxy http.Handler) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "songguo-services", Version: a.version}, nil)
	d := &dispatcher{key: key, proxy: proxy}

	// --- Text-to-Image (openai/images, synchronous) ---
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "text-to-image",
		Description: "Text-to-Image. Generate an image from a prompt via an OpenAI-compatible image endpoint (POST /v1/images/generations), routed by model. Args: prompt, model (required); size?, n?, provider?. Returns the image(s). Metered against your key.",
	}, d.textToImage)

	// --- Text-to-Speech (volc/tts-unidirectional, synchronous) ---
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "text-to-speech",
		Description: "Text-to-Speech. Synthesize speech from text via Volcengine unidirectional TTS (POST /api/v3/tts/unidirectional). Args: text, voice, model (required — sent as the resource id); format? (default mp3), sample_rate? (default 24000), provider?. Returns the audio clip. Metered per character.",
	}, d.textToSpeech)

	// --- Automatic Speech Recognition (volc/asr-file, submit -> poll) ---
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "automatic-speech-recognition",
		Description: "Automatic Speech Recognition (submit). Start async file transcription via Volcengine (POST /api/v3/auc/bigmodel/submit); the audio is fetched by URL. Args: audio_url (required); audio_format? (default wav), provider?. Returns the vendor ack plus the request_id to poll with get-transcription. Pin the same provider on both when several serve this endpoint.",
	}, d.automaticSpeechRecognition)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get-transcription",
		Description: "Automatic Speech Recognition (poll). Fetch the transcript for a prior automatic-speech-recognition submit (POST /api/v3/auc/bigmodel/query). Args: request_id (required, from submit); provider? (pin the submit's provider for affinity). Returns the transcript, or a still-processing status. Metered per second of audio on completion.",
	}, d.getTranscription)

	// --- Text-to-Video (ark/video, submit -> poll) ---
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "text-to-video",
		Description: "Text-to-Video (submit). Start an async video generation task via Volcengine Ark (POST /api/v3/contents/generations/tasks), routed by model. Args: prompt, model (required); provider?. Returns the vendor response including the task id to poll with get-text-to-video. Pin the same provider on both when several serve this endpoint.",
	}, d.textToVideo)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get-text-to-video",
		Description: "Text-to-Video (poll). Fetch the status/result of a prior text-to-video task (GET /api/v3/contents/generations/tasks/{task_id}). Args: task_id, provider (both required). NOTE: the status path is served by unmatched passthrough (it is not a metered wire), so the pinned provider must allow unmatched passthrough; the poll itself meters zero.",
	}, d.getTextToVideo)

	return srv
}

// --- dispatcher: originates native proxy calls carrying the caller's key ---

type dispatcher struct {
	key   string
	proxy http.Handler
}

// call forwards one native request to the transparent proxy in-process and
// returns its status and response bytes. The proxy does the real work — auth,
// routing, credential-swap, metering, verbatim forward. provider (if set) is
// the X-Songguo-Provider pin; extra carries any wire-specific headers.
func (d *dispatcher) call(ctx context.Context, method, path, provider string, extra map[string]string, body []byte) (int, []byte) {
	req := httptest.NewRequest(method, path, bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+d.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if provider != "" {
		req.Header.Set("X-Songguo-Provider", provider)
	}
	for k, v := range extra {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	d.proxy.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// --- Text-to-Image ---

type textToImageArgs struct {
	Prompt   string `json:"prompt" jsonschema:"the text prompt to render"`
	Model    string `json:"model" jsonschema:"the image model id to route to"`
	Size     string `json:"size,omitempty" jsonschema:"image size, e.g. 1024x1024 (vendor default if omitted)"`
	N        int    `json:"n,omitempty" jsonschema:"how many images to generate (vendor default if omitted)"`
	Provider string `json:"provider,omitempty" jsonschema:"pin a specific configured provider id"`
}

type textToImageResult struct {
	Status int    `json:"status"`
	Model  string `json:"model,omitempty"`
	Images int    `json:"images"`
}

func (d *dispatcher) textToImage(ctx context.Context, _ *mcp.CallToolRequest, args textToImageArgs) (*mcp.CallToolResult, textToImageResult, error) {
	if strings.TrimSpace(args.Prompt) == "" || strings.TrimSpace(args.Model) == "" {
		return toolError("prompt and model are required"), textToImageResult{}, nil
	}
	body := map[string]any{"model": args.Model, "prompt": args.Prompt}
	if args.Size != "" {
		body["size"] = args.Size
	}
	if args.N > 0 {
		body["n"] = args.N
	}
	status, resp := d.call(ctx, http.MethodPost, "/v1/images/generations", args.Provider, nil, jsonBody(body))
	if !ok2xx(status) {
		return verbatimError(resp), textToImageResult{Status: status}, nil
	}
	content, n := imageContentFromOpenAI(resp)
	return &mcp.CallToolResult{Content: content}, textToImageResult{Status: status, Model: args.Model, Images: n}, nil
}

// --- Text-to-Speech ---

type textToSpeechArgs struct {
	Text       string `json:"text" jsonschema:"the text to speak"`
	Voice      string `json:"voice" jsonschema:"the speaker/voice id, e.g. zh_female_vv_uranus_bigtts"`
	Model      string `json:"model" jsonschema:"the TTS model id, sent as the X-Api-Resource-Id"`
	Format     string `json:"format,omitempty" jsonschema:"audio format: mp3 (default), wav, ogg, pcm"`
	SampleRate int    `json:"sample_rate,omitempty" jsonschema:"sample rate in Hz (default 24000)"`
	Provider   string `json:"provider,omitempty" jsonschema:"pin a specific configured provider id"`
}

type textToSpeechResult struct {
	Status int `json:"status"`
}

func (d *dispatcher) textToSpeech(ctx context.Context, _ *mcp.CallToolRequest, args textToSpeechArgs) (*mcp.CallToolResult, textToSpeechResult, error) {
	if strings.TrimSpace(args.Text) == "" || strings.TrimSpace(args.Voice) == "" || strings.TrimSpace(args.Model) == "" {
		return toolError("text, voice and model are required"), textToSpeechResult{}, nil
	}
	format := args.Format
	if format == "" {
		format = "mp3"
	}
	sampleRate := args.SampleRate
	if sampleRate == 0 {
		sampleRate = 24000
	}
	body := map[string]any{
		"user": map[string]any{"uid": "songguo-mcp"},
		"req_params": map[string]any{
			"text":         args.Text,
			"speaker":      args.Voice,
			"audio_params": map[string]any{"format": format, "sample_rate": sampleRate},
		},
	}
	extra := map[string]string{
		"X-Api-Resource-Id":                     args.Model,
		"X-Api-Request-Id":                      newRequestID(),
		"X-Control-Require-Usage-Tokens-Return": "true",
	}
	status, resp := d.call(ctx, http.MethodPost, "/api/v3/tts/unidirectional", args.Provider, extra, jsonBody(body))
	if !ok2xx(status) {
		return verbatimError(resp), textToSpeechResult{Status: status}, nil
	}
	if content, okAudio := audioFromVolcNDJSON(resp, mimeForAudio(format)); okAudio {
		return &mcp.CallToolResult{Content: content}, textToSpeechResult{Status: status}, nil
	}
	// Unexpected shape: hand the raw response back rather than hide it.
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(resp)}}}, textToSpeechResult{Status: status}, nil
}

// --- Automatic Speech Recognition (submit) ---

const asrResourceID = "volc.seedasr.auc"

type asrSubmitArgs struct {
	AudioURL    string `json:"audio_url" jsonschema:"URL of the recording to transcribe"`
	AudioFormat string `json:"audio_format,omitempty" jsonschema:"audio container/format, e.g. wav (default), mp3"`
	Provider    string `json:"provider,omitempty" jsonschema:"pin a specific configured provider id (reuse it on the poll)"`
}

type asrSubmitResult struct {
	Status    int    `json:"status"`
	RequestID string `json:"request_id,omitempty"`
	Provider  string `json:"provider,omitempty"`
}

func (d *dispatcher) automaticSpeechRecognition(ctx context.Context, _ *mcp.CallToolRequest, args asrSubmitArgs) (*mcp.CallToolResult, asrSubmitResult, error) {
	if strings.TrimSpace(args.AudioURL) == "" {
		return toolError("audio_url is required"), asrSubmitResult{}, nil
	}
	format := args.AudioFormat
	if format == "" {
		format = "wav"
	}
	requestID := newRequestID()
	body := map[string]any{
		"user":    map[string]any{"uid": "songguo-mcp"},
		"audio":   map[string]any{"url": args.AudioURL, "format": format},
		"request": map[string]any{"model_name": "bigmodel"},
	}
	extra := map[string]string{
		"X-Api-Resource-Id": asrResourceID,
		"X-Api-Request-Id":  requestID,
	}
	status, resp := d.call(ctx, http.MethodPost, "/api/v3/auc/bigmodel/submit", args.Provider, extra, jsonBody(body))
	if !ok2xx(status) {
		return verbatimError(resp), asrSubmitResult{Status: status}, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(resp)}}},
		asrSubmitResult{Status: status, RequestID: requestID, Provider: args.Provider}, nil
}

// --- Automatic Speech Recognition (poll) ---

type asrQueryArgs struct {
	RequestID string `json:"request_id" jsonschema:"the request_id returned by automatic-speech-recognition"`
	Provider  string `json:"provider,omitempty" jsonschema:"the provider pinned on submit, for affinity"`
}

type asrQueryResult struct {
	Status int `json:"status"`
}

func (d *dispatcher) getTranscription(ctx context.Context, _ *mcp.CallToolRequest, args asrQueryArgs) (*mcp.CallToolResult, asrQueryResult, error) {
	if strings.TrimSpace(args.RequestID) == "" {
		return toolError("request_id is required"), asrQueryResult{}, nil
	}
	extra := map[string]string{
		"X-Api-Resource-Id": asrResourceID,
		"X-Api-Request-Id":  args.RequestID,
	}
	status, resp := d.call(ctx, http.MethodPost, "/api/v3/auc/bigmodel/query", args.Provider, extra, jsonBody(map[string]any{}))
	if !ok2xx(status) {
		return verbatimError(resp), asrQueryResult{Status: status}, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(resp)}}}, asrQueryResult{Status: status}, nil
}

// --- Text-to-Video (submit) ---

type videoSubmitArgs struct {
	Prompt   string `json:"prompt" jsonschema:"the text prompt to generate video from"`
	Model    string `json:"model" jsonschema:"the video model id to route to"`
	Provider string `json:"provider,omitempty" jsonschema:"pin a specific configured provider id (reuse it on the poll)"`
}

type videoSubmitResult struct {
	Status   int    `json:"status"`
	TaskID   string `json:"task_id,omitempty"`
	Provider string `json:"provider,omitempty"`
}

func (d *dispatcher) textToVideo(ctx context.Context, _ *mcp.CallToolRequest, args videoSubmitArgs) (*mcp.CallToolResult, videoSubmitResult, error) {
	if strings.TrimSpace(args.Prompt) == "" || strings.TrimSpace(args.Model) == "" {
		return toolError("prompt and model are required"), videoSubmitResult{}, nil
	}
	body := map[string]any{
		"model":   args.Model,
		"content": []any{map[string]any{"type": "text", "text": args.Prompt}},
	}
	status, resp := d.call(ctx, http.MethodPost, "/api/v3/contents/generations/tasks", args.Provider, nil, jsonBody(body))
	if !ok2xx(status) {
		return verbatimError(resp), videoSubmitResult{Status: status}, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(resp)}}},
		videoSubmitResult{Status: status, TaskID: jsonStringField(resp, "id"), Provider: args.Provider}, nil
}

// --- Text-to-Video (poll) ---

type videoQueryArgs struct {
	TaskID   string `json:"task_id" jsonschema:"the task id returned by text-to-video"`
	Provider string `json:"provider" jsonschema:"the provider pinned on submit (required — the status path is provider-specific passthrough)"`
}

type videoQueryResult struct {
	Status int `json:"status"`
}

func (d *dispatcher) getTextToVideo(ctx context.Context, _ *mcp.CallToolRequest, args videoQueryArgs) (*mcp.CallToolResult, videoQueryResult, error) {
	if strings.TrimSpace(args.TaskID) == "" || strings.TrimSpace(args.Provider) == "" {
		return toolError("task_id and provider are required"), videoQueryResult{}, nil
	}
	path := "/api/v3/contents/generations/tasks/" + args.TaskID
	status, resp := d.call(ctx, http.MethodGet, path, args.Provider, nil, nil)
	if !ok2xx(status) {
		return verbatimError(resp), videoQueryResult{Status: status}, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(resp)}}}, videoQueryResult{Status: status}, nil
}

// --- shared helpers ---

func ok2xx(status int) bool { return status >= 200 && status < 300 }

func jsonBody(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// newRequestID returns a random hex id for the X-Api-Request-Id the caller owns
// across a submit->poll pair (the Volcengine task key).
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// jsonStringField best-effort reads a top-level string field from a JSON object
// body (e.g. the task "id" in a video submit response). Returns "" if absent.
func jsonStringField(body []byte, key string) string {
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		if v, ok := m[key].(string); ok {
			return v
		}
	}
	return ""
}

// verbatimError surfaces a non-2xx proxy/vendor response as an MCP tool error
// (IsError, body as text) so the agent sees exactly what came back.
func verbatimError(body []byte) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}
}

// toolError builds an MCP tool-level error result (IsError, message as text) so
// the model sees the failure and can self-correct.
func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
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
	for _, item := range parsed.Data {
		switch {
		case item.B64JSON != "":
			if raw, err := base64.StdEncoding.DecodeString(item.B64JSON); err == nil {
				out = append(out, &mcp.ImageContent{Data: raw, MIMEType: "image/png"})
			}
		case item.URL != "":
			out = append(out, &mcp.TextContent{Text: item.URL})
		}
	}
	if len(out) == 0 {
		return []mcp.Content{&mcp.TextContent{Text: string(body)}}, 0
	}
	return out, len(out)
}

// audioFromVolcNDJSON reassembles a Volcengine unidirectional-TTS response —
// newline-delimited JSON chunks each carrying a base64 "data" audio fragment —
// into a single MCP AudioContent. Returns false if no audio was found.
func audioFromVolcNDJSON(body []byte, mime string) ([]mcp.Content, bool) {
	var audio []byte
	found := false
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var chunk struct {
			Data string `json:"data"`
		}
		if json.Unmarshal(line, &chunk) != nil || chunk.Data == "" {
			continue
		}
		if raw, err := base64.StdEncoding.DecodeString(chunk.Data); err == nil {
			audio = append(audio, raw...)
			found = true
		}
	}
	if !found {
		return nil, false
	}
	return []mcp.Content{&mcp.AudioContent{Data: audio, MIMEType: mime}}, true
}

func mimeForAudio(format string) string {
	switch strings.ToLower(format) {
	case "wav":
		return "audio/wav"
	case "ogg":
		return "audio/ogg"
	case "pcm":
		return "audio/pcm"
	default:
		return "audio/mpeg" // mp3
	}
}
