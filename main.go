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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var (
	currentConfig config
	startTime     = time.Now()
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	currentConfig = cfg
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

	srv := &server{pool: p, client: client, sem: make(chan struct{}, currentConfig.MaxConcurrent)}

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", currentConfig.ListenPort),
		Handler:           srv.routes(),
		ReadHeaderTimeout: 15 * time.Second,
		// No WriteTimeout: a streaming answer can legitimately run for minutes.
		ErrorLog: log.New(io.Discard, "", 0),
	}

	go func() {
		log.Printf("qwen2api listening on :%d (max_concurrent=%d)", currentConfig.ListenPort, currentConfig.MaxConcurrent)
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
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/models", s.withAuth(s.handleModels))
	mux.HandleFunc("/models", s.handleModels) // mirrors upstream's unauthenticated alias
	mux.HandleFunc("/v1/chat/completions", s.withAuth(s.handleChat))
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

	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  startTime.Unix(),
			"owned_by": "qwen",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// chatRequest is the accepted inbound OpenAI body. Unknown fields are ignored.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   *bool         `json:"stream"`
	// EnableThinking / ThinkingBudget are Qwen extensions upstream supports.
	EnableThinking *bool `json:"enable_thinking"`
	ThinkingBudget int   `json:"thinking_budget"`
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
	var req chatRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "malformed JSON body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "messages is required and must be non-empty")
		return
	}
	model := req.Model
	if model == "" {
		model = "qwen3.6-plus"
	}

	stream := req.Stream != nil && *req.Stream

	// Acquire a concurrency slot before doing any upstream work.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-r.Context().Done():
		return
	default:
		writeJSONError(w, http.StatusTooManyRequests, "rate_limit_error", "too many concurrent requests")
		return
	}

	acct, err := s.pool.acquire(r.Context())
	if err != nil {
		status := http.StatusServiceUnavailable
		writeJSONError(w, status, "server_error", err.Error())
		return
	}

	chatType := "t2t"
	chatID, err := s.client.createChatID(r.Context(), acct, model, chatType)
	if err != nil {
		var ue *upstreamError
		if errors.As(err, &ue) && (ue.Status == 401 || ue.Status == 403) {
			s.pool.invalidate(acct)
		}
		log.Printf("chat: create session failed for %s: %v", acct.Email, err)
		writeJSONError(w, http.StatusBadGateway, "server_error", "could not open an upstream conversation: "+err.Error())
		return
	}

	payload, err := buildUpstreamPayload(req, model, chatID, chatType)
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
		s.streamResponse(w, r, upstream, model)
		return
	}
	s.bufferedResponse(w, r, upstream, model)
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

	finish := "stop"
	if acc.failed {
		finish = "stop"
	}
	writeJSON(w, http.StatusOK, json.RawMessage(
		responseJSON(completionID(), model, time.Now().Unix(), acc.content.String(), acc.reasoning.String(), finish, acc.usage)))
}

// streamResponse relays the answer as OpenAI SSE chunks.
func (s *server) streamResponse(w http.ResponseWriter, r *http.Request, upstream *response, model string) {
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

	err := consumeUpstream(r.Context(), upstream.Body, acc)
	if err != nil && !errors.Is(err, errClientGone) {
		log.Printf("chat: stream failed: %v", err)
		// Surface the failure inside the stream; a truncated answer that looks
		// complete is worse than an explicit error.
		writeJSONError(w, 0, "server_error", err.Error())
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
	content   strings.Builder
	reasoning strings.Builder
	usage     upstreamUsage
	failed    bool
	wrote     bool
}

func newStreamAccumulator() *streamAccumulator { return &streamAccumulator{} }

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
	if !strings.HasPrefix(data, "{") {
		return nil
	}

	var root map[string]any
	if err := json.Unmarshal([]byte(data), &root); err != nil {
		// A partial or non-JSON frame is not fatal on its own; skip it.
		return nil
	}

	// Upstream reports business failures inside the stream, sometimes with HTTP
	// 200. Treat them as hard errors rather than silently returning nothing.
	if msg, ok := upstreamFailure(root); ok {
		acc.failed = true
		return errors.New(msg)
	}

	acc.usage.merge(root["usage"])

	choices, _ := root["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
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
		return nil
	}

	acc.wrote = true
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
func upstreamFailure(root map[string]any) (string, bool) {
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

// --- upstream request body ------------------------------------------------

// buildUpstreamPayload assembles the body the Qwen web client sends.
//
// Two details are load-bearing:
//   - feature_config.output_schema must be "phase". Without it the upstream stops
//     emitting delta.phase and every incremental frame is discarded.
//   - The web endpoint accepts exactly one current message, so a multi-turn
//     conversation is flattened: earlier turns go into a JSONL history block and
//     the final turn becomes the current message.
func buildUpstreamPayload(req chatRequest, model, chatID, chatType string) ([]byte, error) {
	now := time.Now().Unix()
	content := flattenMessages(req.Messages)

	thinking := false
	if req.EnableThinking != nil {
		thinking = *req.EnableThinking
	}
	if strings.Contains(model, "-thinking") {
		thinking = true
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
		"messages": []map[string]any{{
			"id":            nil,
			"fid":           uuidv4(),
			"parentId":      nil,
			"parent_id":     nil,
			"childrenIds":   []string{uuidv4()},
			"role":          "user",
			"content":       content,
			"user_action":   "chat",
			"files":         []any{},
			"timestamp":     now,
			"models":        []string{model},
			"model":         "",
			"chat_type":     chatType,
			"sub_chat_type": chatType,
			"feature_config": map[string]any{
				"output_schema":    "phase",
				"thinking_enabled": thinking,
				"research_mode":    "normal",
				"auto_thinking":    true,
				"thinking_mode":    "Auto",
				"thinking_format":  "summary",
				"auto_search":      true,
			},
			"extra": map[string]any{"meta": map[string]any{"subChatType": chatType}},
		}},
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

// --- small helpers --------------------------------------------------------

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
