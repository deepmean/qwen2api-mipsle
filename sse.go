package main

// SSE decoding and OpenAI wire-format mapping.
//
// Ported from Qwen2API src/utils/sse.js (SSEDecoder/parseSSEFrame),
// src/utils/chat-helpers.js#createUpstreamDeltaNormalizer, and the OpenAI chunk
// emitters in src/controllers/chat.js.
//
// Upstream always streams and always uses SSE, so there is no non-streaming path
// to fall back to; a non-streaming client is served by consuming the whole stream
// and emitting one complete JSON body.

import (
	"encoding/json"
	"strings"
)

// --- SSE decoding ---------------------------------------------------------

type sseFrame struct {
	Event string
	Data  string
}

// sseDecoder reassembles SSE frames from arbitrary TCP chunk boundaries. A frame
// can be split mid-UTF-8-character, mid-field, or mid-JSON, so decoding is done
// on the raw byte stream with an explicit remainder buffer.
type sseDecoder struct {
	buf strings.Builder
}

// push feeds a chunk and returns every complete frame it completed.
func (d *sseDecoder) push(chunk []byte) []sseFrame {
	if len(chunk) > 0 {
		d.buf.Write(chunk)
	}
	return d.drain(false)
}

// end flushes a trailing frame that arrived without its terminating blank line.
func (d *sseDecoder) end() []sseFrame {
	return d.drain(true)
}

func (d *sseDecoder) drain(flush bool) []sseFrame {
	raw := d.buf.String()
	var frames []sseFrame

	for {
		idx, sepLen := findFrameBoundary(raw)
		if idx < 0 {
			break
		}
		frame, ok := parseSSEFrame(raw[:idx])
		if ok {
			frames = append(frames, frame)
		}
		raw = raw[idx+sepLen:]
	}

	if flush && strings.TrimSpace(raw) != "" {
		if frame, ok := parseSSEFrame(raw); ok {
			frames = append(frames, frame)
		}
		raw = ""
	}

	d.buf.Reset()
	d.buf.WriteString(raw)
	return frames
}

// findFrameBoundary locates the next blank-line separator and its length.
func findFrameBoundary(s string) (int, int) {
	if i := strings.Index(s, "\r\n\r\n"); i >= 0 {
		if j := strings.Index(s, "\n\n"); j >= 0 && j < i {
			return j, 2
		}
		return i, 4
	}
	if i := strings.Index(s, "\n\n"); i >= 0 {
		return i, 2
	}
	return -1, 0
}

// parseSSEFrame decodes one frame. Qwen occasionally answers HTTP 200 with a
// bare JSON object instead of an SSE data field (WAF/business failure), so a
// frame with no fields at all but valid JSON is returned as a data frame rather
// than silently dropped.
func parseSSEFrame(raw string) (sseFrame, bool) {
	var out sseFrame
	var dataLines []string
	hasField := false

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		var field, value string
		if ci := strings.IndexByte(line, ':'); ci < 0 {
			field = line
		} else {
			field = line[:ci]
			value = line[ci+1:]
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "data":
			dataLines = append(dataLines, value)
			hasField = true
		case "event":
			out.Event = value
			hasField = true
		case "id", "retry":
			hasField = true
		}
	}

	if !hasField {
		trimmed := strings.TrimSpace(raw)
		if (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && isJSON(trimmed) {
			out.Data = trimmed
			return out, true
		}
		return out, false
	}

	out.Data = strings.Join(dataLines, "\n")
	return out, true
}

func isJSON(s string) bool {
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}

// --- upstream delta normalisation ----------------------------------------

// answerPhases mirrors ANSWER_PHASES in tool-prompt.js.
var answerPhases = map[string]bool{
	"answer": true, "final": true, "final_answer": true, "response": true,
}

func isThinkPhase(phase string) bool {
	return phase == "think" || phase == "thinking" || phase == "thinking_summary"
}

// deltaNormalizer collapses Qwen's delta variants onto a two-phase model.
//
// It carries state because reasoning can arrive on two channels:
//
//   - `phase: think|thinking` with `reasoning_content`, and
//   - `phase: thinking_summary`, where the text lives in
//     `extra.summary_thought.content` as an ever-growing array. Only the entries
//     past the previous high-water mark are new, so the count has to persist
//     across frames or the reasoning is re-emitted on every tick.
//
// The summary channel is the one this build actually gets: the upstream request
// asks for `thinking_format: "summary"`, matching the web client.
type deltaNormalizer struct {
	summaryThoughtCount int
}

// normalize returns ok=false for frames carrying no user-visible text, including
// the upstream's own tool-registry injections (role:"function"), which are never
// the assistant's answer.
func (n *deltaNormalizer) normalize(d map[string]any) (phase, content string, ok bool) {
	if d == nil {
		return "", "", false
	}
	if role, _ := d["role"].(string); role == "function" {
		return "", "", false
	}

	rawPhase, _ := d["phase"].(string)
	rc, hasRC := stringField(d, "reasoning_content")
	c, hasC := stringField(d, "content")

	useReasoning := hasRC && !answerPhases[rawPhase]

	if !isThinkPhase(rawPhase) && !answerPhases[rawPhase] && !hasRC && !hasC {
		return "", "", false
	}

	switch {
	case useReasoning:
		content = rc
	case hasC:
		content = c
	}

	// The summary channel overrides whatever content the frame also carried.
	if rawPhase == "thinking_summary" {
		thoughts := summaryThoughts(d)
		if len(thoughts) > n.summaryThoughtCount {
			content = strings.Join(thoughts[n.summaryThoughtCount:], "\n")
			n.summaryThoughtCount = len(thoughts)
		} else {
			content = ""
		}
	}

	if content == "" {
		return "", "", false
	}

	if isThinkPhase(rawPhase) || useReasoning {
		phase = "think"
	} else {
		phase = "answer"
	}
	return phase, content, true
}

// summaryThoughts extracts extra.summary_thought.content, dropping empty and
// non-string entries (upstream's `.filter(Boolean)`).
func summaryThoughts(d map[string]any) []string {
	extra, ok := d["extra"].(map[string]any)
	if !ok {
		return nil
	}
	st, ok := extra["summary_thought"].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := st["content"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok && s != ""
}

// upstreamUsage accumulates Qwen's cumulative DashScope-style counters. Each
// typing frame carries running totals, so the latest non-zero value wins.
type upstreamUsage struct {
	InputTokens  int
	OutputTokens int
}

func (u *upstreamUsage) merge(raw any) {
	m, ok := raw.(map[string]any)
	if !ok {
		return
	}
	if v, ok := intField(m, "input_tokens"); ok {
		u.InputTokens = v
	}
	if v, ok := intField(m, "output_tokens"); ok {
		u.OutputTokens = v
	}
}

func intField(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

// --- OpenAI wire format ---------------------------------------------------

type openAIDelta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type openAIChoice struct {
	Index        int          `json:"index"`
	Delta        *openAIDelta `json:"delta,omitempty"`
	Message      *openAIMsg   `json:"message,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

type openAIMsg struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openAIChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

type openAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

func (u upstreamUsage) toOpenAI() *openAIUsage {
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return nil
	}
	return &openAIUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
}

// chunkJSON renders one streaming chunk.
func chunkJSON(id, model string, created int64, delta *openAIDelta, finish *string) []byte {
	c := openAIChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []openAIChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	}
	b, _ := json.Marshal(c)
	return b
}

// responseJSON renders a complete non-streaming body.
func responseJSON(id, model string, created int64, content, reasoning string, finish string, u upstreamUsage) []byte {
	r := openAIResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []openAIChoice{{
			Index:        0,
			Message:      &openAIMsg{Role: "assistant", Content: content, ReasoningContent: reasoning},
			FinishReason: &finish,
		}},
		Usage: u.toOpenAI(),
	}
	b, _ := json.Marshal(r)
	return b
}
