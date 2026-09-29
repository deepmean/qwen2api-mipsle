package main

// Upstream request-body budgeting.
//
// Qwen's edge (Aliyun WAF) refuses a request body at roughly 128 KiB and answers
// with a "user validate" challenge instead of a completion. An agent client like
// Claude Code resends its whole conversation every turn, so it crosses that line
// within a few exchanges and every subsequent turn fails — with an empty reply,
// because the challenge arrives as a normal HTTP 200.
//
// Upstream Qwen2API solves this by externalising the context into a Qwen document
// attachment once the body crosses a threshold (AGENT_CONTEXT_FILE_THRESHOLD_BYTES,
// default 90 KiB). Attachments are not implemented here, so this port compacts
// instead: the system prompt and the most recent turns are kept in full, and older
// turns are dropped with an explicit notice so the model knows its history is
// partial.
//
// Compacting changes what the model sees, which is a real trade-off: a long
// session loses early context. It is still strictly better than a guaranteed
// empty reply, and the notice makes the loss visible rather than silent.

import (
	"encoding/json"
	"strings"
)

// wafBodyLimitBytes is the size past which Qwen's edge starts challenging.
// Measured on device: 124,965 B completes normally, 127,965 B is challenged.
const wafBodyLimitBytes = 128 * 1024

// defaultUpstreamBudgetBytes leaves headroom under the WAF limit for the JSON
// envelope, the session fields and any upstream-side counting differences.
const defaultUpstreamBudgetBytes = 90 * 1024

// systemPromptBudget caps how much of the system prompt survives compaction.
// It is generous because the system prompt defines the agent's behaviour, but it
// must not be able to consume the whole budget on its own.
const systemPromptBudget = 16 * 1024

// envelopeOverheadBytes covers the JSONL headers and framing that
// flattenMessages adds around the messages themselves.
const envelopeOverheadBytes = 512

// messageCost measures a message exactly as flattenMessages will render it: one
// JSON object per line. Estimating from the raw text instead would undercount
// the quoting and escaping, and the request would still cross the WAF ceiling.
func messageCost(m chatMessage) int {
	return len(mustJSON(map[string]string{"role": m.Role, "content": m.text()})) + 1
}

// compactMessages trims a conversation to fit a byte budget.
//
// It keeps the system prompt (truncated if oversized) and as many of the most
// recent messages as fit, dropping the oldest. A notice is prepended so the
// model can tell that earlier context was removed rather than never existing.
// Returns the compacted slice and the number of messages dropped.
func compactMessages(msgs []chatMessage, budget int) ([]chatMessage, int) {
	if budget <= 0 || len(msgs) == 0 {
		return msgs, 0
	}

	var system []chatMessage
	rest := msgs
	if msgs[0].Role == "system" {
		system = msgs[:1]
		rest = msgs[1:]
	}

	// The system prompt is truncated first: it is a fixed cost that would
	// otherwise squeeze out every turn.
	sysBytes := 0
	if len(system) > 0 {
		text := system[0].text()
		if len(text) > systemPromptBudget {
			text = truncateMiddle(text, systemPromptBudget)
			system = []chatMessage{{Role: "system", Content: mustJSON(text)}}
		}
		sysBytes = len(text)
	}

	notice := "[earlier conversation omitted: the request exceeded the upstream size limit, " +
		"so older turns were dropped to keep the most recent ones]"
	noticeCost := len(notice) + 64 // JSON framing for the notice message
	used := sysBytes + noticeCost + envelopeOverheadBytes

	// Walk newest-to-oldest, keeping whole messages. A half-truncated message is
	// worse than a missing one: it reads as a complete thought that the model
	// then builds on.
	kept := make([]chatMessage, 0, len(rest))
	for i := len(rest) - 1; i >= 0; i-- {
		n := messageCost(rest[i])
		if used+n > budget && len(kept) > 0 {
			break
		}
		kept = append(kept, rest[i])
		used += n
		if used >= budget {
			break
		}
	}

	// Reverse back into chronological order.
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}

	dropped := len(rest) - len(kept)
	if dropped <= 0 && sysBytes+len(notice) <= budget {
		// Nothing was actually removed; leave the conversation untouched.
		return msgs, 0
	}

	out := make([]chatMessage, 0, len(system)+len(kept)+1)
	out = append(out, system...)
	out = append(out, chatMessage{Role: "user", Content: mustJSON(notice)})
	out = append(out, kept...)
	return out, dropped
}

// truncateMiddle keeps the head and tail of a string, which is where a system
// prompt usually carries its instructions and its most recent additions.
func truncateMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := max * 6 / 10
	tail := max - head
	return s[:head] + "\n...[truncated]...\n" + s[len(s)-tail:]
}

func mustJSON(v any) []byte {
	b, err := jsonMarshal(v)
	if err != nil {
		return []byte(`""`)
	}
	return b
}

// flattenedSize reports the byte size of the conversation once rendered into the
// single-message envelope, which is what the upstream actually receives.
func flattenedSize(msgs []chatMessage) int {
	return len(flattenMessages(msgs)) + len(strings.Join([]string{"# Conversation history (JSONL)", "# Current message"}, ""))
}

// jsonMarshal is a thin indirection so context.go does not import encoding/json
// just for two call sites.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
