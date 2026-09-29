package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func rawMsg(role, content string) chatMessage {
	b, _ := json.Marshal(content)
	return chatMessage{Role: role, Content: b}
}

func TestRenderExternalizedFitsBudgetWithLargeSystem(t *testing.T) {
	// A system prompt far larger than the whole budget must be truncated so the
	// rendered body still fits — this is the 2 MB Claude Code shape.
	env := contextEnvelope{
		prefix:  strings.Repeat("rule ", 40000), // ~200 KB
		history: `{"role":"user","content":"hi"}`,
		current: currentMarker + "\n" + `{"role":"user","content":"go"}`,
	}
	out := renderExternalized(env, "QWEN2API_AGENT_HISTORY_1.txt", 1, "")
	if !fitsBudget(out, 1) {
		t.Fatalf("rendered body does not fit: %d bytes", len(out))
	}
	if !strings.Contains(out, "[truncated]") {
		t.Fatal("oversized system prompt should have been truncated")
	}
	if !strings.Contains(out, "# Agent context attachment") {
		t.Fatal("attachment notice missing")
	}
	if !strings.Contains(out, historyMarker) {
		t.Fatal("history marker missing")
	}
}

func TestRenderExternalizedCarriesTail(t *testing.T) {
	env := contextEnvelope{
		prefix:  "sys",
		history: "l1\nl2\nl3\nl4",
		current: currentMarker + "\n" + `{"role":"user","content":"go"}`,
	}
	out := renderExternalized(env, "f.txt", 2, "l3\nl4")
	if !strings.Contains(out, "l3\nl4") {
		t.Fatal("tail lines should be inline")
	}
	if !strings.Contains(out, "continues from message 3") {
		t.Fatalf("tail line should point past the attachment: %q", out)
	}
	if !strings.Contains(out, "sys") {
		t.Fatal("system prefix should be inline")
	}
}

func TestRenderExternalizedFitsWithLargeCurrent(t *testing.T) {
	// A large current message must not be truncated away — it is the actual task.
	current := currentMarker + "\n" + `{"role":"user","content":"` + strings.Repeat("x", 40000) + `"}`
	env := contextEnvelope{prefix: "sys", history: "h", current: current}
	out := renderExternalized(env, "f.txt", 1, "")
	if !strings.Contains(out, strings.Repeat("x", 40000)) {
		t.Fatal("current message was truncated")
	}
}

func TestFitsBudgetAccountsForDescriptors(t *testing.T) {
	inline := strings.Repeat("a", defaultUpstreamBudgetBytes-1024)
	if !fitsBudget(inline, 0) {
		t.Fatal("inline alone should fit")
	}
	if fitsBudget(inline, 1) {
		t.Fatal("inline plus a descriptor should not fit")
	}
}
