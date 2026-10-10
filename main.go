package main

// qwen2api — a minimal Qwen web → OpenAI-compatible gateway.
//
// Ported to Go for the JCG-Q20 (ramips/mt7621, OpenWrt 24.10). The upstream
// project (github.com/deepmean/Qwen2API) is a Bun/Node application; neither Bun
// nor Node exists for MIPS32, so the core chat path was reimplemented here.
//
// Scope: GET /v1/models, POST /v1/chat/completions (stream and non-stream),
// GET /health. Everything else upstream ships — Anthropic compatibility, the
// tool-calling protocol, image/video generation, attachment upload and the web
// admin panel — is deliberately out of scope to keep the resident set small.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	currentConfig config
	startTime     = time.Now()
	// debugRawFrames dumps every upstream SSE frame verbatim. Enabled by
	// LOG_LEVEL=DEBUG; it carries answer text, so it is off by default.
	debugRawFrames bool
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	currentConfig = cfg
	debugRawFrames = cfg.LogLevel == "DEBUG"
	if cfg.AccountsFile == "" {
		currentConfig.AccountsFile = "/opt/qwen2api/accounts.txt"
	}

	accounts, err := loadAccounts(currentConfig.AccountsFile)
	if err != nil {
		log.Fatalf("accounts: %v", err)
	}

	client := &qwenClient{hc: newHTTPClient()}
	p := newPool(accounts, client)

	if len(accounts) == 0 {
		log.Printf("WARN: no accounts loaded from %s — /health will report unavailable", currentConfig.AccountsFile)
	} else {
		log.Printf("loaded %d account(s) from %s", len(accounts), currentConfig.AccountsFile)
	}
	if len(currentConfig.APIKeys) == 0 {
		log.Printf("WARN: API_KEY is unset — /v1/* is open to anyone who can reach this port")
	}

	srv := &server{
		pool:     p,
		client:   client,
		sem:      make(chan struct{}, currentConfig.MaxConcurrent),
		imageSem: make(chan struct{}, currentConfig.ImageMaxConcurrent),
		prefix:   newPrefixCache(time.Duration(currentConfig.PrefixCacheTTLSec)*time.Second, currentConfig.PrefixCacheMaxEntries),
	}

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", currentConfig.ListenPort),
		Handler:           srv.routes(),
		ReadHeaderTimeout: 15 * time.Second,
		// No WriteTimeout: a streaming answer can legitimately run for minutes.
		ErrorLog: log.New(io.Discard, "", 0),
	}

	go func() {
		log.Printf("qwen2api listening on :%d (max_concurrent=%d image_max_concurrent=%d)",
			currentConfig.ListenPort, currentConfig.MaxConcurrent, currentConfig.ImageMaxConcurrent)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	// procd sends SIGTERM on stop; drain briefly so in-flight answers can finish.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	log.Printf("stopped")
}

type server struct {
	pool   *pool
	client *qwenClient
	// sem bounds concurrent chat requests. The router has ~80MB free RAM and
	// each streamed answer holds its own buffers.
	sem chan struct{}

	// imageSem bounds image-generation requests separately. A t2i answer is just
	// a URL on the ordinary stream, so it is cheap; the cost is the b64_json
	// download, which pulls the PNG into RAM and base64-expands it. Kept at 2 by
	// default because the measured OOM point on this device was 4 concurrent
	// multi-megabyte buffers.
	imageSem chan struct{}

	// prefix caches uploaded history attachments so a parse covers many turns.
	prefix *prefixCache
	// breaker pauses uploads after the WAF repeatedly challenges the parse call.
	breaker parseBreaker

	// modelsMu guards the catalogue cache, which is refreshed at most once per
	// modelCacheTTL so a chat request does not pay for a discovery round trip.
	modelsMu sync.Mutex
	models   []modelInfo
	modelsAt time.Time
}

// modelCacheTTL bounds how stale the catalogue may be. Upstream defaults to an
// hour; the list changes rarely and a stale entry only costs a missed alias.
const modelCacheTTL = time.Hour

// cachedModels returns the last known catalogue, refreshing it when stale. A
// fetch failure is not fatal: alias resolution simply falls back to the stripped
// name, and the previous cache is kept.
func (s *server) cachedModels() []modelInfo {
	s.modelsMu.Lock()
	models, at := s.models, s.modelsAt
	s.modelsMu.Unlock()

	if len(models) > 0 && time.Since(at) < modelCacheTTL {
		return models
	}
	if s.pool.empty() {
		return models
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	acct, err := s.pool.acquire(ctx)
	if err != nil {
		return models
	}
	fetched, err := s.client.listModels(ctx, acct)
	if err != nil || len(fetched) == 0 {
		return models
	}

	s.modelsMu.Lock()
	s.models, s.modelsAt = fetched, time.Now()
	s.modelsMu.Unlock()
	return fetched
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/models", s.withAuth(s.handleModels))
	mux.HandleFunc("/models", s.handleModels) // mirrors upstream's unauthenticated alias
	mux.HandleFunc("/v1/chat/completions", s.withAuth(s.handleChat))
	mux.HandleFunc("/v1/images/generations", s.withAuth(s.handleImageGeneration))
	return mux
}

// --- auth -----------------------------------------------------------------

func (s *server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// No keys configured means the operator has not locked the port down;
		// the startup warning is the control in that case.
		if len(currentConfig.APIKeys) == 0 {
			next(w, r)
			return
		}
		key := r.Header.Get("Authorization")
		if key == "" {
			key = r.Header.Get("x-api-key")
		}
		key = strings.TrimPrefix(key, "Bearer ")
		key = strings.TrimSpace(key)

		for _, valid := range currentConfig.APIKeys {
			if subtleEqual(key, valid) {
				next(w, r)
				return
			}
		}
		writeJSONError(w, http.StatusUnauthorized, "invalid_request_error", "Incorrect API key provided")
	}
}

// subtleEqual compares without an early exit, so key length/prefix is not
// recoverable from response timing.
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// --- handlers -------------------------------------------------------------

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"uptime_sec":    int(time.Since(startTime).Seconds()),
		"accounts":      s.pool.size(),
		"account_state": s.pool.health(),
	})
}

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	models := fallbackModels
	if !s.pool.empty() {
		acct, err := s.pool.acquire(ctx)
		if err == nil {
			if fetched, err := s.client.listModels(ctx, acct); err == nil && len(fetched) > 0 {
				models = fetched
			} else if err != nil {
				log.Printf("models: falling back to static list: %v", err)
			}
		}
	}

	// Upstream lists only bare ids, so a client asking for `qwen3.8-max-image`
	// never sees it advertised. The suffix is how this gateway and the upstream
	// web client both express "use the image capability of this model", so the
	// variants are synthesised here from the model's declared chat_types — the
	// same capability check the upstream metadata carries.
	expanded := make([]modelInfo, 0, len(models)*2)
	for _, m := range models {
		expanded = append(expanded, m)
		if hasChatType(m.ChatTypes, "t2i") {
			expanded = append(expanded, modelInfo{ID: m.ID + "-image", Name: m.Name + "-image", ChatTypes: m.ChatTypes})
		}
	}

	data := make([]map[string]any, 0, len(expanded))
	for _, m := range expanded {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  startTime.Unix(),
			"owned_by": "qwen",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// hasChatType reports whether the upstream metadata advertises a capability.
func hasChatType(types []string, want string) bool {
	for _, t := range types {
		if strings.EqualFold(strings.TrimSpace(t), want) {
			return true
		}
	}
	return false
}

// chatRequest is the accepted inbound OpenAI body. Unknown fields are ignored.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   *bool         `json:"stream"`
	// System is the top-level system prompt some clients send instead of (or in
	// addition to) a system message. Anthropic-shaped requests carry it here.
	System json.RawMessage `json:"system"`
	// EnableThinking / ThinkingBudget are Qwen extensions upstream supports.
	EnableThinking *bool `json:"enable_thinking"`
	ThinkingBudget int   `json:"thinking_budget"`
	// Size selects the output aspect ratio for image requests ("1:1", "16:9",
	// ...). Ignored on the text path.
	Size string `json:"size"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (m chatMessage) text() string {
	if len(m.Content) == 0 {
		return ""
	}
	// Content is either a plain string or an array of parts; concatenate the
	// text parts and ignore the rest (images are out of scope for the core build).
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Type == "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	// Log every inbound turn at entry. Without this, a request that never
	// reaches the upstream is indistinguishable from one that was never sent,
	// which is exactly the ambiguity that makes "empty reply" reports hard to
	// diagnose. The size is logged; the body itself is not, since it carries the
	// user's prompts.
	rawBody, err := readAllLimited(r.Body, 4<<20)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "could not read body: "+err.Error())
		return
	}
	log.Printf("chat: <- %s %s (%d bytes) ua=%q", r.Method, r.URL.Path, len(rawBody), r.Header.Get("user-agent"))

	var req chatRequest
	dec := json.NewDecoder(bytes.NewReader(rawBody))
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "malformed JSON body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "messages is required and must be non-empty")
		return
	}
	// Fold a top-level `system` field into the conversation when the messages do
	// not already start with one, so the system prompt is treated uniformly
	// (kept inline and budgeted) regardless of which shape the client sent.
	if len(req.System) > 0 && req.Messages[0].Role != "system" {
		req.Messages = append([]chatMessage{{Role: "system", Content: req.System}}, req.Messages...)
	}
	model := req.Model
	if model == "" {
		model = "qwen3.6-plus"
	}

	stream := req.Stream != nil && *req.Stream

	// Acquire a concurrency slot before doing any upstream work. Image requests
	// take their own, tighter gate as well: the generated picture is a URL on the
	// stream (cheap), but a b64_json caller makes us download and expand it, and
	// that is the allocation this device cannot do many times at once.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-r.Context().Done():
		return
	default:
		writeJSONError(w, http.StatusTooManyRequests, "rate_limit_error", "too many concurrent requests")
		return
	}

	// Normalise the inbound name before anything else: clients decorate it with
	// a `[..]` context marker and behavioural suffixes that upstream rejects.
	resolved := resolveModel(model, s.cachedModels(), req.EnableThinking != nil && *req.EnableThinking)
	if resolved.Unsupported != "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error",
			"this build does not support "+resolved.Unsupported+" (model "+model+")")
		return
	}

	if resolved.ChatType == "t2i" {
		select {
		case s.imageSem <- struct{}{}:
			defer func() { <-s.imageSem }()
		case <-r.Context().Done():
			return
		default:
			writeJSONError(w, http.StatusTooManyRequests, "rate_limit_error",
				"too many concurrent image requests")
			return
		}
	}

	acct, err := s.pool.acquire(r.Context())
	if err != nil {
		status := http.StatusServiceUnavailable
		writeJSONError(w, status, "server_error", err.Error())
		return
	}

	if resolved.ID != model {
		log.Printf("chat: model %q -> %q (chat_type=%s thinking=%v)", model, resolved.ID, resolved.ChatType, resolved.Thinking)
	}
	// Always report what was resolved, so a silent upstream (200 + empty stream)
	// can be traced back to the exact name that was sent.
	log.Printf("chat: model=%q resolved=%q chat_type=%s thinking=%v msgs=%d stream=%v",
		model, resolved.ID, resolved.ChatType, resolved.Thinking, len(req.Messages), stream)

	// Keep the body under the WAF's ~128 KiB ceiling. Agent clients resend their
	// whole conversation each turn, so without this every turn past the limit is
	// answered with a challenge (HTTP 200, no text). Prefer moving the history
	// into a document attachment (lossless); fall back to compaction only when
	// uploads are unavailable.
	//
	// An image request is a single prompt, not a conversation, and attaching a
	// document to it would confuse the t2i controller — so it goes inline.
	var ext externalizeResult
	if resolved.ChatType == "t2i" {
		ext = externalizeResult{content: flattenMessages(req.Messages), mode: "inline"}
	} else {
		ext = s.externalizeContext(r.Context(), acct, req.Messages, resolved.ID)
	}

	chatID, err := s.client.createChatID(r.Context(), acct, resolved.ID, resolved.ChatType)
	if err != nil {
		var ue *upstreamError
		if errors.As(err, &ue) && (ue.Status == 401 || ue.Status == 403) {
			s.pool.invalidate(acct)
		}
		log.Printf("chat: create session failed for %s: %v", acct.Email, err)
		writeJSONError(w, http.StatusBadGateway, "server_error", "could not open an upstream conversation: "+err.Error())
		return
	}

	payload, err := buildUpstreamPayloadSized(resolved, chatID, ext, normalizeImageSize(req.Size))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	upstream, err := s.client.chatStream(r.Context(), acct, chatID, payload)
	if err != nil {
		log.Printf("chat: upstream request failed for %s: %v", acct.Email, err)
		writeJSONError(w, http.StatusBadGateway, "server_error", err.Error())
		return
	}
	defer upstream.Close()

	if upstream.StatusCode == 401 || upstream.StatusCode == 403 {
		s.pool.invalidate(acct)
		body, _ := io.ReadAll(io.LimitReader(upstream.Body, 4<<10))
		log.Printf("chat: upstream rejected token for %s: %d %s", acct.Email, upstream.StatusCode, strings.TrimSpace(string(body)))
		writeJSONError(w, http.StatusBadGateway, "server_error", "upstream rejected the account token")
		return
	}
	if upstream.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(upstream.Body, 4<<10))
		log.Printf("chat: upstream %d for %s: %s", upstream.StatusCode, acct.Email, strings.TrimSpace(string(body)))
		writeJSONError(w, http.StatusBadGateway, "server_error",
			fmt.Sprintf("upstream returned %d", upstream.StatusCode))
		return
	}

	if stream {
		s.streamResponse(w, r, upstream, resolved.ID)
		return
	}
	s.bufferedResponse(w, r, upstream, resolved.ID)
}

// completionID is the OpenAI-style identifier attached to every chunk of a turn.
func completionID() string { return "chatcmpl-" + strings.ReplaceAll(uuidv4(), "-", "") }

// --- response paths -------------------------------------------------------

// bufferedResponse drains the whole upstream stream and emits one JSON body.
func (s *server) bufferedResponse(w http.ResponseWriter, r *http.Request, upstream *response, model string) {
	acc := newStreamAccumulator()
	if err := consumeUpstream(r.Context(), upstream.Body, acc); err != nil {
		log.Printf("chat: stream failed: %v", err)
		if !acc.wroteAnything() {
			writeJSONError(w, http.StatusBadGateway, "server_error", err.Error())
			return
		}
	}

	// A t2i answer has no prose: the payload is the picture URL. Emit it as the
	// message body so a caller that asked for an image through the chat endpoint
	// receives the link instead of an empty string.
	content := acc.content.String()
	if imgs := acc.images; len(imgs) > 0 {
		if content != "" {
			content += "\n"
		}
		content += strings.Join(imgs, "\n")
	}

	finish := "stop"
	if acc.failed {
		finish = "stop"
	}
	writeJSON(w, http.StatusOK, json.RawMessage(
		responseJSON(completionID(), model, time.Now().Unix(), content, acc.reasoning.String(), finish, acc.usage)))
}

// streamResponse relays the answer as OpenAI SSE chunks.
func (s *server) streamResponse(w http.ResponseWriter, r *http.Request, upstream *response, model string) {
	streamStart := time.Now()
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.bufferedResponse(w, r, upstream, model)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Defeat proxy buffering, which would otherwise hold the whole answer back.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	id := completionID()
	created := time.Now().Unix()
	acc := newStreamAccumulator()

	write := func(payload []byte) bool {
		if _, err := w.Write([]byte("data: ")); err != nil {
			return false
		}
		if _, err := w.Write(payload); err != nil {
			return false
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Opening chunk carries the role, as OpenAI clients expect.
	if !write(chunkJSON(id, model, created, &openAIDelta{Role: "assistant"}, nil)) {
		return
	}

	acc.onDelta = func(phase, content string) error {
		var delta *openAIDelta
		if phase == "think" {
			delta = &openAIDelta{ReasoningContent: content}
		} else {
			delta = &openAIDelta{Content: content}
		}
		if !write(chunkJSON(id, model, created, delta, nil)) {
			return errClientGone
		}
		return nil
	}
	// An image answer is a URL, not prose. It is captured separately by the
	// accumulator, so relay it here as content: a streaming client that asked for
	// an image must not see an empty stream.
	acc.onImage = func(url string) error {
		if !write(chunkJSON(id, model, created, &openAIDelta{Content: url}, nil)) {
			return errClientGone
		}
		return nil
	}

	err := consumeUpstream(r.Context(), upstream.Body, acc)
	if err != nil && !errors.Is(err, errClientGone) {
		log.Printf("chat: stream failed: %v", err)
		// Surface the failure inside the stream; a truncated answer that looks
		// complete is worse than an explicit error.
		writeJSONError(w, 0, "server_error", err.Error())
	}
	log.Printf("chat: -> %d bytes content, %d bytes reasoning, frames=%d/%d raw, err=%v, %s",
		acc.content.Len(), acc.reasoning.Len(), acc.frames, acc.raw, err, time.Since(streamStart).Round(time.Millisecond))
	for i, s := range acc.rawSamples {
		log.Printf("chat:   unusable frame[%d] %s", i, s)
	}

	finish := "stop"
	if err != nil && !errors.Is(err, errClientGone) {
		finish = "stop"
	}
	stop := finish
	write(chunkJSON(id, model, created, &openAIDelta{}, &stop))

	if usage := acc.usage.toOpenAI(); usage != nil {
		final := openAIChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: []openAIChoice{}, Usage: usage}
		if b, err := json.Marshal(final); err == nil {
			write(b)
		}
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}

var errClientGone = errors.New("client disconnected")

// --- upstream stream consumption -----------------------------------------

// streamAccumulator turns normalised upstream deltas into either a relayed SSE
// chunk (onDelta) or accumulated buffers (non-streaming path).
type streamAccumulator struct {
	onDelta   func(phase, content string) error
	norm      deltaNormalizer
	filter    upstreamResponseFilter
	content   strings.Builder
	reasoning strings.Builder
	usage     upstreamUsage
	// images collects generated picture URLs. Upstream tags these frames with
	// phase "image_gen" and puts the URL on the ordinary content field, so they
	// are separated here from the text answer rather than mixed into it.
	images []string
	// onImage relays a captured picture URL to a streaming client. It is separate
	// from onDelta because an image is not prose.
	onImage func(url string) error
	// frames counts upstream SSE frames that carried a choice, and raw counts
	// every frame seen — the pair distinguishes "upstream sent nothing" from
	// "upstream sent frames this build does not recognise".
	frames int
	raw    int
	// rawSamples keeps the first few frames that produced no delta, so an empty
	// answer can be explained instead of guessed at.
	rawSamples []string
	failed     bool
	wrote      bool
}

func newStreamAccumulator() *streamAccumulator { return &streamAccumulator{} }

// sample records an unrecognised frame for the end-of-turn log, keeping only the
// first few so a chatty upstream cannot grow the buffer without limit.
func (a *streamAccumulator) sample(s string) {
	const maxSamples = 3
	const maxLen = 500
	if len(a.rawSamples) >= maxSamples {
		return
	}
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	a.rawSamples = append(a.rawSamples, s)
}

func (a *streamAccumulator) wroteAnything() bool { return a.wrote }

// consumeUpstream reads the SSE body to completion, invoking the accumulator.
func consumeUpstream(ctx context.Context, body io.Reader, acc *streamAccumulator) error {
	dec := &sseDecoder{}
	buf := make([]byte, 16<<10)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, err := body.Read(buf)
		if n > 0 {
			for _, frame := range dec.push(buf[:n]) {
				if ferr := handleFrame(frame, acc); ferr != nil {
					return ferr
				}
			}
		}
		if err == io.EOF {
			for _, frame := range dec.end() {
				if ferr := handleFrame(frame, acc); ferr != nil {
					return ferr
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func handleFrame(frame sseFrame, acc *streamAccumulator) error {
	data := strings.TrimSpace(frame.Data)
	if data == "" || data == "[DONE]" {
		return nil
	}
	acc.raw++
	if !strings.HasPrefix(data, "{") {
		acc.sample(data)
		return nil
	}

	var root map[string]any
	if err := json.Unmarshal([]byte(data), &root); err != nil {
		// A partial or non-JSON frame is not fatal on its own; skip it.
		acc.sample(data)
		return nil
	}
	if debugRawFrames {
		log.Printf("chat: RAW %s", truncateForLog(data, 1200))
	}

	// Upstream reports business failures inside the stream, sometimes with HTTP
	// 200. Treat them as hard errors rather than silently returning nothing.
	if msg, ok := upstreamFailure(root); ok {
		acc.failed = true
		return errors.New(msg)
	}

	// Drop frames from non-primary answer candidates. Upstream runs several
	// candidates concurrently and interleaves them; consuming them all
	// concatenates two different answers into one garbled reply.
	if !acc.filter.accept(root) {
		return nil
	}

	acc.usage.merge(root["usage"])

	choices, _ := root["choices"].([]any)
	if len(choices) == 0 {
		// A frame with no choices is either a keepalive or the shape upstream
		// uses to report a refused request; record it so the two are separable.
		acc.sample(data)
		return nil
	}
	acc.frames++
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return nil
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return nil
	}

	phase, content, ok := acc.norm.normalize(delta)
	if !ok {
		if b, err := json.Marshal(delta); err == nil {
			acc.sample(string(b))
		}
		return nil
	}

	acc.wrote = true
	// A generated picture arrives as a URL on `content` under phase "image_gen".
	// Keep it out of the text answer so a client reading `content` does not get a
	// bare URL where prose is expected, and so the image endpoint can find it.
	if p, _ := delta["phase"].(string); p == imagePhase {
		url := strings.TrimSpace(content)
		acc.images = append(acc.images, url)
		if acc.onImage != nil && url != "" {
			return acc.onImage(url)
		}
		return nil
	}
	if phase == "think" {
		acc.reasoning.WriteString(content)
	} else {
		acc.content.WriteString(content)
	}
	if acc.onDelta != nil {
		return acc.onDelta(phase, content)
	}
	return nil
}

// upstreamFailure recognises the error envelopes Qwen returns inside a 200
// stream. The shapes vary, so several key spellings are checked.
//
// The most important one is the WAF challenge. Qwen's edge answers an oversized
// or suspicious request with HTTP 200 and a body like:
//
//	{"ret":["FAIL_SYS_USER_VALIDATE","RGV587_ERROR::SM::..."],
//	 "data":{"url":"https://chat.qwen.ai/.../_____tmd_____/punish?x5secdata=..."}}
//
// Left unrecognised it looks like a stream that simply produced no text, so the
// caller returns a successful but empty answer. Detecting it turns a silent
// failure into an explicit one.
func upstreamFailure(root map[string]any) (string, bool) {
	if msg, ok := wafChallenge(root); ok {
		return msg, true
	}
	if errObj, ok := root["error"].(map[string]any); ok {
		if msg, ok := errObj["message"].(string); ok && msg != "" {
			return msg, true
		}
		if code, ok := errObj["code"].(string); ok && code != "" {
			return code, true
		}
		return "upstream reported an error", true
	}
	if code, ok := root["code"].(string); ok && code != "" && code != "200" {
		if msg, ok := root["message"].(string); ok && msg != "" {
			return code + ": " + msg, true
		}
		return code, true
	}
	if status, ok := root["status"].(string); ok && status == "failed" {
		if msg, ok := root["message"].(string); ok && msg != "" {
			return msg, true
		}
		return "upstream status failed", true
	}
	return "", false
}

// wafChallenge detects the Aliyun WAF "user validate" envelope. The `ret` field
// is an array whose first element names the verdict.
func wafChallenge(root map[string]any) (string, bool) {
	ret, ok := root["ret"].([]any)
	if !ok || len(ret) == 0 {
		return "", false
	}
	code, _ := ret[0].(string)
	if !strings.HasPrefix(code, "FAIL_SYS_USER_VALIDATE") && !strings.HasPrefix(code, "FAIL_SYS_") {
		return "", false
	}
	detail := code
	if len(ret) > 1 {
		if d, ok := ret[1].(string); ok && d != "" {
			detail = d
		}
	}
	return "upstream WAF challenge: " + detail, true
}

// --- upstream request body ------------------------------------------------

// buildUpstreamPayload assembles the body the Qwen web client sends.
//
// Two details are load-bearing:
//   - feature_config.output_schema must be "phase". Without it the upstream stops
//     emitting delta.phase and every incremental frame is discarded.
//   - The web endpoint accepts exactly one current message, so a multi-turn
//     conversation is flattened: earlier turns go into a JSONL history block and
//     the final turn becomes the current message.
func buildUpstreamPayload(resolved resolvedModel, chatID string, ext externalizeResult) ([]byte, error) {
	return buildUpstreamPayloadSized(resolved, chatID, ext, "")
}

// buildUpstreamPayloadSized is buildUpstreamPayload plus an optional asset size
// ("1:1", "16:9", ...). Size is only meaningful for the image chat types.
func buildUpstreamPayloadSized(resolved resolvedModel, chatID string, ext externalizeResult, size string) ([]byte, error) {
	now := time.Now().Unix()
	model := resolved.ID
	chatType := resolved.ChatType
	files := ext.files
	if files == nil {
		files = []any{}
	}

	// An image request is verified against upstream as a single-prompt, no-search,
	// no-thinking turn; the text path keeps the agent-oriented defaults.
	feature := map[string]any{
		"output_schema":    "phase",
		"thinking_enabled": resolved.Thinking,
		"research_mode":    "normal",
		"auto_thinking":    true,
		"thinking_mode":    "Auto",
		"thinking_format":  "summary",
		"auto_search":      true,
	}
	meta := map[string]any{"subChatType": chatType}
	if chatType == "t2i" {
		feature["thinking_enabled"] = false
		feature["auto_thinking"] = false
		feature["thinking_mode"] = "Fast"
		feature["auto_search"] = false
		delete(feature, "thinking_format")
	}
	if size != "" {
		meta["size"] = size
	}

	message := map[string]any{
		"id":            nil,
		"fid":           uuidv4(),
		"parentId":      nil,
		"parent_id":     nil,
		"childrenIds":   []string{uuidv4()},
		"role":          "user",
		"content":       ext.content,
		"user_action":   "chat",
		"files":         files,
		"timestamp":     now,
		"models":        []string{model},
		"model":         "",
		"chat_type":     chatType,
		"sub_chat_type": chatType,
		"feature_config": feature,
		"extra":         map[string]any{"meta": meta},
	}
	if size != "" {
		message["size"] = size
	}

	body := map[string]any{
		"stream":             true,
		"version":            "2.1",
		"incremental_output": true,
		"chat_id":            chatID,
		"chatId":             chatID,
		"chat_mode":          "normal",
		"model":              model,
		"parent_id":          nil,
		"parentId":           nil,
		"timestamp":          now,
		"messages":           []map[string]any{message},
	}
	return json.Marshal(body)
}

// flattenMessages renders the conversation into the single-message form the web
// endpoint expects. A lone message is passed through verbatim; anything longer
// becomes a JSONL envelope so roles and newlines survive intact.
func flattenMessages(msgs []chatMessage) string {
	if len(msgs) == 1 {
		return msgs[0].text()
	}

	var history strings.Builder
	for _, m := range msgs[:len(msgs)-1] {
		line, err := json.Marshal(map[string]string{"role": m.Role, "content": m.text()})
		if err != nil {
			continue
		}
		history.Write(line)
		history.WriteByte('\n')
	}

	last := msgs[len(msgs)-1]
	current, _ := json.Marshal(map[string]string{"role": last.Role, "content": last.text()})

	var b strings.Builder
	if h := strings.TrimRight(history.String(), "\n"); h != "" {
		b.WriteString("# Conversation history (JSONL)\n")
		b.WriteString(h)
		b.WriteString("\n")
	}
	if s := strings.TrimSpace(last.text()); s != "" {
		b.WriteString("# Current message\n")
		b.Write(append([]byte(nil), current...))
	}
	return b.String()
}

// --- OpenAI images API ----------------------------------------------------

// imageRequest is the OpenAI /v1/images/generations body.
type imageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
}

// handleImageGeneration serves POST /v1/images/generations by running a t2i turn
// upstream and returning the URL of the generated picture.
//
// The work is a single prompt, so it reuses the chat path's transport but skips
// the conversation machinery. b64_json downloads and expands the PNG, which is
// the allocation that forced a separate concurrency gate on this device.
func (s *server) handleImageGeneration(w http.ResponseWriter, r *http.Request) {
	rawBody, err := readAllLimited(r.Body, 64<<10)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "could not read body: "+err.Error())
		return
	}
	var req imageRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "malformed JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "qwen3.8-max"
	}

	// One image request occupies both the global gate and the tighter image gate.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-r.Context().Done():
		return
	default:
		writeJSONError(w, http.StatusTooManyRequests, "rate_limit_error", "too many concurrent requests")
		return
	}
	select {
	case s.imageSem <- struct{}{}:
		defer func() { <-s.imageSem }()
	case <-r.Context().Done():
		return
	default:
		writeJSONError(w, http.StatusTooManyRequests, "rate_limit_error", "too many concurrent image requests")
		return
	}

	resolved := resolveModel(model, s.cachedModels(), false)
	resolved.ChatType = "t2i"

	acct, err := s.pool.acquire(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "server_error", err.Error())
		return
	}

	chatID, err := s.client.createChatID(r.Context(), acct, resolved.ID, "t2i")
	if err != nil {
		var ue *upstreamError
		if errors.As(err, &ue) && (ue.Status == 401 || ue.Status == 403) {
			s.pool.invalidate(acct)
		}
		log.Printf("images: create session failed for %s: %v", acct.Email, err)
		writeJSONError(w, http.StatusBadGateway, "server_error", "could not open an upstream conversation: "+err.Error())
		return
	}

	ext := externalizeResult{content: req.Prompt, mode: "inline"}
	payload, err := buildUpstreamPayloadSized(resolved, chatID, ext, normalizeImageSize(req.Size))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	upstream, err := s.client.chatStream(r.Context(), acct, chatID, payload)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "server_error", err.Error())
		return
	}
	defer upstream.Close()

	if upstream.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(upstream.Body, 4<<10))
		log.Printf("images: upstream %d for %s: %s", upstream.StatusCode, acct.Email, strings.TrimSpace(string(body)))
		writeJSONError(w, http.StatusBadGateway, "server_error", fmt.Sprintf("upstream returned %d", upstream.StatusCode))
		return
	}

	acc := newStreamAccumulator()
	if err := consumeUpstream(r.Context(), upstream.Body, acc); err != nil {
		log.Printf("images: stream failed: %v", err)
	}
	if len(acc.images) == 0 {
		log.Printf("images: no image URL in upstream stream (frames=%d raw=%d)", acc.frames, acc.raw)
		writeJSONError(w, http.StatusBadGateway, "server_error", "upstream produced no image")
		return
	}
	url := acc.images[len(acc.images)-1]
	log.Printf("images: generated for %s model=%s url_bytes=%d", acct.Email, resolved.ID, len(url))

	item := map[string]any{"url": url}
	if req.ResponseFormat == "b64_json" {
		if encoded, err := s.downloadImageBase64(r.Context(), url); err == nil {
			item = map[string]any{"b64_json": encoded}
		} else {
			log.Printf("images: b64 download failed, returning url instead: %v", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"created": time.Now().Unix(),
		"data":    []map[string]any{item},
	})
}

// downloadImageBase64 fetches a generated picture and base64-encodes it. The
// read is bounded by ImageMaxBytes: base64 expands by 4/3, and this device was
// measured OOM-killing at a handful of multi-megabyte buffers.
func (s *server) downloadImageBase64(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("asset fetch returned %d", resp.StatusCode)
	}

	limit := currentConfig.ImageMaxBytes
	if limit <= 0 {
		limit = 8 << 20
	}
	body, err := readAllLimited(resp.Body, int(limit)+1)
	if err != nil {
		return "", err
	}
	if int64(len(body)) > limit {
		return "", fmt.Errorf("asset exceeds IMAGE_MAX_BYTES (%d)", limit)
	}
	return base64.StdEncoding.EncodeToString(body), nil
}

// --- small helpers --------------------------------------------------------

// normalizeImageSize maps the pixel sizes OpenAI clients send onto the aspect
// ratios Qwen expects, and passes an already-ratio value through unchanged.
func normalizeImageSize(size string) string {
	switch strings.TrimSpace(size) {
	case "1024x1024":
		return "1:1"
	case "1536x1024":
		return "4:3"
	case "1024x1536":
		return "3:4"
	case "1792x1024":
		return "16:9"
	case "1024x1792":
		return "9:16"
	default:
		return strings.TrimSpace(size)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":{"message":"encoding failure"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeJSONError emits the OpenAI error envelope. A status of 0 means the
// response has already started (mid-stream), so only the body is written.
func writeJSONError(w http.ResponseWriter, status int, errType, message string) {
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": errType},
	})
	if status == 0 {
		_, _ = w.Write(append(append([]byte("data: "), payload...), []byte("\n\n")...))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}
