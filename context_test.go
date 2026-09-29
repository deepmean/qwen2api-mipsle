package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func raw(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func TestCompactMessagesDropsOldestAndKeepsRecent(t *testing.T) {
	msgs := []chatMessage{{Role: "system", Content: raw("be brief")}}
	for i := 0; i < 100; i++ {
		msgs = append(msgs, chatMessage{Role: "user", Content: raw(strings.Repeat("x", 500) + " turn" + string(rune('0'+i%10)))})
	}

	out, dropped := compactMessages(msgs, 8*1024)
	if dropped == 0 {
		t.Fatal("expected messages to be dropped")
	}
	if len(out) >= len(msgs) {
		t.Fatalf("compaction did not shrink the conversation: %d -> %d", len(msgs), len(out))
	}

	// The system prompt survives.
	if out[0].Role != "system" {
		t.Fatalf("first message is %q, want system", out[0].Role)
	}
	// A notice makes the truncation visible to the model.
	if !strings.Contains(out[1].text(), "omitted") {
		t.Fatalf("missing omission notice: %q", out[1].text())
	}
	// The newest message is preserved: the model must see the current turn.
	last := msgs[len(msgs)-1].text()
	if out[len(out)-1].text() != last {
		t.Fatalf("last message lost: got %q want %q", out[len(out)-1].text(), last)
	}
}

func TestCompactMessagesLeavesSmallConversationsAlone(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: raw("sys")},
		{Role: "user", Content: raw("hello")},
	}
	out, dropped := compactMessages(msgs, 64*1024)
	if dropped != 0 || len(out) != len(msgs) {
		t.Fatalf("small conversation was modified: dropped=%d len=%d", dropped, len(out))
	}
}

func TestCompactMessagesTruncatesOversizedSystemPrompt(t *testing.T) {
	huge := strings.Repeat("s", 64*1024)
	msgs := []chatMessage{
		{Role: "system", Content: raw(huge)},
		{Role: "user", Content: raw("current question")},
	}
	out, _ := compactMessages(msgs, 8*1024)
	if got := len(out[0].text()); got > systemPromptBudget+64 {
		t.Fatalf("system prompt not truncated: %d bytes", got)
	}
	// The tail of the prompt is the most recently added instruction, so it must
	// survive the middle-out truncation.
	if !strings.HasSuffix(out[0].text(), "s") {
		t.Fatal("system prompt tail was lost")
	}
	if out[len(out)-1].text() != "current question" {
		t.Fatal("current turn was lost")
	}
}

func TestCompactMessagesHandlesNoSystemPrompt(t *testing.T) {
	var msgs []chatMessage
	for i := 0; i < 50; i++ {
		msgs = append(msgs, chatMessage{Role: "user", Content: raw(strings.Repeat("y", 400))})
	}
	out, dropped := compactMessages(msgs, 4*1024)
	if dropped == 0 {
		t.Fatal("expected dropping")
	}
	if out[0].Role != "user" {
		t.Fatalf("first message is %q, want user (no system prompt to preserve)", out[0].Role)
	}
}

func TestTruncateMiddle(t *testing.T) {
	s := strings.Repeat("a", 100) + strings.Repeat("b", 100)
	got := truncateMiddle(s, 40)
	if len(got) > 60 {
		t.Fatalf("not truncated enough: %d bytes", len(got))
	}
	if !strings.HasPrefix(got, "aaaa") || !strings.HasSuffix(got, "bbbb") {
		t.Fatalf("head or tail lost: %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatal("missing truncation marker")
	}
}

// The regression this file exists for: a body over the WAF ceiling must be
// compacted before it is sent, or the upstream answers with a challenge.
func TestFlattenedSizeUnderBudgetAfterCompaction(t *testing.T) {
	var msgs []chatMessage
	msgs = append(msgs, chatMessage{Role: "system", Content: raw(strings.Repeat("s", 3000))})
	for i := 0; i < 400; i++ {
		msgs = append(msgs, chatMessage{Role: "user", Content: raw(strings.Repeat("m", 600))})
	}
	before := flattenedSize(msgs)
	if before <= defaultUpstreamBudgetBytes {
		t.Fatalf("fixture is too small to exercise compaction: %d", before)
	}

	out, dropped := compactMessages(msgs, defaultUpstreamBudgetBytes)
	if dropped == 0 {
		t.Fatal("expected compaction")
	}
	after := flattenedSize(out)
	if after > defaultUpstreamBudgetBytes {
		t.Fatalf("still over budget after compaction: %d > %d", after, defaultUpstreamBudgetBytes)
	}
}

func TestWAFChallengeDetection(t *testing.T) {
	// The exact envelope observed from Qwen's edge on an oversized request.
	body := `{"ret":["FAIL_SYS_USER_VALIDATE","RGV587_ERROR::SM::哎呦喂,被挤爆啦"],
	          "data":{"url":"https://chat.qwen.ai:443//api/v2/chat/completions/_____tmd_____/punish?x5secdata=abc"}}`
	var root map[string]any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	msg, ok := upstreamFailure(root)
	if !ok {
		t.Fatal("WAF challenge was not detected")
	}
	if !strings.Contains(msg, "WAF") {
		t.Fatalf("message does not identify the cause: %q", msg)
	}

	// A normal typing frame must not be mistaken for a challenge.
	var normal map[string]any
	json.Unmarshal([]byte(`{"choices":[{"delta":{"content":"hi"}}]}`), &normal)
	if _, ok := upstreamFailure(normal); ok {
		t.Fatal("normal frame flagged as a failure")
	}

	// A ret array that is not a system failure must not trip the detector.
	var other map[string]any
	json.Unmarshal([]byte(`{"ret":["SUCCESS"]}`), &other)
	if _, ok := upstreamFailure(other); ok {
		t.Fatal("non-failure ret flagged")
	}
}
