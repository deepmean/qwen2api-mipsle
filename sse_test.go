package main

import (
	"encoding/json"
	"testing"
)

func TestSSEDecoderHandlesSplitChunks(t *testing.T) {
	dec := &sseDecoder{}

	// A frame delivered one byte at a time must still parse exactly once.
	frame := "data: {\"a\":1}\n\n"
	var got []sseFrame
	for i := 0; i < len(frame); i++ {
		got = append(got, dec.push([]byte{frame[i]})...)
	}
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1: %+v", len(got), got)
	}
	if got[0].Data != `{"a":1}` {
		t.Fatalf("data = %q", got[0].Data)
	}
}

func TestSSEDecoderCRLFAndMultiLineData(t *testing.T) {
	dec := &sseDecoder{}
	chunk := "data: line1\r\ndata: line2\r\n\r\n"
	frames := dec.push([]byte(chunk))
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	// Multiple data lines join with \n, per the SSE spec.
	if frames[0].Data != "line1\nline2" {
		t.Fatalf("data = %q, want %q", frames[0].Data, "line1\nline2")
	}
}

func TestSSEDecoderIgnoresComments(t *testing.T) {
	dec := &sseDecoder{}
	frames := dec.push([]byte(": keepalive\n\ndata: real\n\n"))
	if len(frames) != 1 || frames[0].Data != "real" {
		t.Fatalf("frames = %+v", frames)
	}
}

func TestSSEDecoderBareJSONFrame(t *testing.T) {
	// Qwen's WAF sometimes answers 200 with a bare JSON body instead of SSE.
	dec := &sseDecoder{}
	frames := dec.push([]byte(`{"error":{"message":"challenge"}}`))
	// No blank line yet, so nothing is emitted until end().
	if len(frames) != 0 {
		t.Fatalf("expected no frames before flush, got %+v", frames)
	}
	frames = dec.end()
	if len(frames) != 1 {
		t.Fatalf("got %d frames on flush, want 1", len(frames))
	}
	if !isJSON(frames[0].Data) {
		t.Fatalf("expected JSON data, got %q", frames[0].Data)
	}
}

func TestNormalizeDeltaPhases(t *testing.T) {
	cases := []struct {
		name      string
		delta     map[string]any
		wantPhase string
		wantText  string
		wantOK    bool
	}{
		{"think by phase", map[string]any{"phase": "think", "content": "hmm"}, "think", "hmm", true},
		{"answer by phase", map[string]any{"phase": "answer", "content": "hi"}, "answer", "hi", true},
		{"reasoning_content implies think", map[string]any{"reasoning_content": "r"}, "think", "r", true},
		{"no phase defaults to answer", map[string]any{"content": "x"}, "answer", "x", true},
		{"final is an answer phase", map[string]any{"phase": "final", "content": "x"}, "answer", "x", true},
		{"function role is dropped", map[string]any{"role": "function", "content": "tool"}, "", "", false},
		{"empty delta is dropped", map[string]any{"phase": "answer"}, "", "", false},
		{"nil delta is dropped", nil, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var n deltaNormalizer
			phase, text, ok := n.normalize(tc.delta)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if phase != tc.wantPhase || text != tc.wantText {
				t.Fatalf("got (%q,%q), want (%q,%q)", phase, text, tc.wantPhase, tc.wantText)
			}
		})
	}
}

// summaryDelta builds a thinking_summary frame carrying the given thoughts.
func summaryDelta(thoughts ...string) map[string]any {
	arr := make([]any, 0, len(thoughts))
	for _, t := range thoughts {
		arr = append(arr, t)
	}
	return map[string]any{
		"phase": "thinking_summary",
		"extra": map[string]any{"summary_thought": map[string]any{"content": arr}},
	}
}

func TestThinkingSummaryEmitsOnlyNewThoughts(t *testing.T) {
	var n deltaNormalizer

	// First tick: two thoughts are new.
	phase, text, ok := n.normalize(summaryDelta("step one", "step two"))
	if !ok || phase != "think" || text != "step one\nstep two" {
		t.Fatalf("first tick = (%q,%q,%v)", phase, text, ok)
	}

	// Second tick repeats both and adds one. Only the new entry may be emitted,
	// otherwise the reasoning is re-sent on every frame.
	phase, text, ok = n.normalize(summaryDelta("step one", "step two", "step three"))
	if !ok || phase != "think" || text != "step three" {
		t.Fatalf("second tick = (%q,%q,%v)", phase, text, ok)
	}

	// Third tick with no growth emits nothing.
	if _, _, ok = n.normalize(summaryDelta("step one", "step two", "step three")); ok {
		t.Fatal("no new thoughts should emit nothing")
	}
}

func TestThinkingSummaryIgnoresEmptyEntries(t *testing.T) {
	var n deltaNormalizer
	// Upstream filters falsy entries; an empty string must not count toward the
	// high-water mark.
	phase, text, ok := n.normalize(summaryDelta("real", ""))
	if !ok || phase != "think" || text != "real" {
		t.Fatalf("got (%q,%q,%v)", phase, text, ok)
	}
}

func TestHandleFrameAccumulates(t *testing.T) {
	acc := newStreamAccumulator()
	frames := []string{
		`{"choices":[{"delta":{"phase":"think","content":"let me "}}]}`,
		`{"choices":[{"delta":{"phase":"think","content":"think"}}]}`,
		`{"choices":[{"delta":{"phase":"answer","content":"4"}}],"usage":{"input_tokens":7,"output_tokens":2}}`,
	}
	for _, f := range frames {
		if err := handleFrame(sseFrame{Data: f}, acc); err != nil {
			t.Fatalf("handleFrame: %v", err)
		}
	}
	if acc.reasoning.String() != "let me think" {
		t.Fatalf("reasoning = %q", acc.reasoning.String())
	}
	if acc.content.String() != "4" {
		t.Fatalf("content = %q", acc.content.String())
	}
	if acc.usage.InputTokens != 7 || acc.usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v", acc.usage)
	}
}

func TestHandleFrameSurfacesUpstreamError(t *testing.T) {
	acc := newStreamAccumulator()
	// A 200 response carrying an error envelope must not look like an empty answer.
	err := handleFrame(sseFrame{Data: `{"error":{"message":"rate limited"}}`}, acc)
	if err == nil {
		t.Fatal("expected an error from the error envelope")
	}
	if !acc.failed {
		t.Fatal("accumulator should be marked failed")
	}
}

func TestFlattenMessages(t *testing.T) {
	raw := func(s string) json.RawMessage {
		b, _ := json.Marshal(s)
		return b
	}

	single := []chatMessage{{Role: "user", Content: raw("hello")}}
	if got := flattenMessages(single); got != "hello" {
		t.Fatalf("single message = %q, want %q", got, "hello")
	}

	multi := []chatMessage{
		{Role: "system", Content: raw("be brief")},
		{Role: "user", Content: raw("hi")},
		{Role: "assistant", Content: raw("hello")},
		{Role: "user", Content: raw("what is 2+2?")},
	}
	got := flattenMessages(multi)
	for _, want := range []string{
		"# Conversation history (JSONL)",
		`{"content":"be brief","role":"system"}`,
		`{"content":"hi","role":"user"}`,
		"# Current message",
		`{"content":"what is 2+2?","role":"user"}`,
	} {
		if !contains(got, want) {
			t.Errorf("flattened output missing %q\n---\n%s", want, got)
		}
	}
}

func TestFlattenMessagesArrayContent(t *testing.T) {
	m := chatMessage{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"part one "},{"type":"text","text":"part two"}]`)}
	if got := flattenMessages([]chatMessage{m}); got != "part one part two" {
		t.Fatalf("array content = %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// --- upstream candidate filter -------------------------------------------

func created(id string, index any) map[string]any {
	return map[string]any{"response.created": map[string]any{
		"response_id":    id,
		"response_index": index,
	}}
}

func TestFilterLocksOntoPrimaryCandidate(t *testing.T) {
	var f upstreamResponseFilter
	// Two candidates announced; index 0 is primary.
	if !f.accept(created("A", "0")) {
		t.Fatal("primary announcement rejected")
	}
	// The backup announcement is dropped once the primary is locked.
	if f.accept(created("B", "1")) {
		t.Fatal("backup announcement must be dropped after the primary locks")
	}
	// Body frames are now locked to A.
	if !f.accept(map[string]any{"response_id": "A"}) {
		t.Fatal("primary body frame rejected")
	}
	if f.accept(map[string]any{"response_id": "B"}) {
		t.Fatal("backup body frame must be dropped")
	}
}

func TestFilterPrefersPrimaryEvenWhenBackupArrivesFirst(t *testing.T) {
	var f upstreamResponseFilter
	// Backup candidate announces first — it must NOT take the lock.
	f.accept(created("B", "1"))
	if f.accept(map[string]any{"response_id": "B"}) {
		t.Fatal("backup must not be accepted before the primary is known")
	}
	// Primary announces later and takes the lock.
	f.accept(created("A", "0"))
	if !f.accept(map[string]any{"response_id": "A"}) {
		t.Fatal("primary body frame rejected")
	}
}

func TestFilterPassesFramesWithoutResponseID(t *testing.T) {
	var f upstreamResponseFilter
	if !f.accept(map[string]any{"choices": []any{}}) {
		t.Fatal("frames without a response_id must pass")
	}
	// A keepalive with no id is never filtered even after a lock.
	f.accept(created("A", "0"))
	if !f.accept(map[string]any{"usage": map[string]any{}}) {
		t.Fatal("id-less frame must pass after lock")
	}
}

func TestFilterLegacyProtocolLocksFirstID(t *testing.T) {
	var f upstreamResponseFilter
	// No response.created at all: lock onto the first body frame with an id.
	if !f.accept(map[string]any{"response_id": "X"}) {
		t.Fatal("first legacy frame must be accepted")
	}
	if !f.accept(map[string]any{"response_id": "X"}) {
		t.Fatal("same legacy id must keep passing")
	}
	if f.accept(map[string]any{"response_id": "Y"}) {
		t.Fatal("different legacy id must be dropped")
	}
}

func TestFilterIndexAsNumberAndCamelCase(t *testing.T) {
	var f upstreamResponseFilter
	// Numeric index (not a string) and camelCase id key.
	f.accept(map[string]any{"response.created": map[string]any{
		"responseId":    "A",
		"responseIndex": 0,
	}})
	if !f.accept(map[string]any{"response_id": "A"}) {
		t.Fatal("numeric index 0 should lock the candidate")
	}
	if f.accept(map[string]any{"response_id": "B"}) {
		t.Fatal("other id must be dropped")
	}
}
