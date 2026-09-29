package main

// Long-context externalization: keep the upstream request body small by moving the
// conversation history into a Qwen document attachment.
//
// Ported from Qwen2API src/utils/request.js#externalizeOversizedAgentContext and
// src/utils/context-prefix-cache.js.
//
// Qwen's edge rejects a body at roughly 128 KiB (see context.go), and an agent
// client resends its whole conversation every turn. Instead of dropping history
// — which loses context — the history is uploaded once as a document and then
// referenced from message.files. The model reads the attachment, so nothing is
// lost.
//
// Parsing a document is the call the WAF rate-limits, so the uploaded history is
// cached per session and reused: when this turn's history starts with the lines
// that were baked into the cached document, only the new tail is sent inline and
// no upload happens. One parse covers many turns.
//
// Compaction (context.go) remains as the fallback for when an upload fails.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	historyMarker = "# Conversation history (JSONL)"
	currentMarker = "# Current message"

	// attachmentNamePrefix matches upstream's HISTORY_ATTACHMENT_NAME_PREFIX.
	attachmentNamePrefix = "QWEN2API_AGENT_HISTORY_"
)

// contextEnvelope is the flattened message content split into its three parts.
// The layout matches what upstream's parseAgentEnvelope expects, so a body built
// here can be parsed back:
//
//	<prefix>
//	# Conversation history (JSONL)
//	<history lines>
//	# Current message
//	{current}
type contextEnvelope struct {
	prefix  string // system/instructions before the history block
	history string // the JSONL history block, without its marker
	current string // the current message block, including its marker
}

// buildEnvelope splits a conversation into the envelope form. The last message is
// the current one; everything before it is history. A leading system message is
// kept as the prefix so it stays inline (it defines the agent's behaviour) while
// the history is externalized.
func buildEnvelope(msgs []chatMessage) contextEnvelope {
	if len(msgs) == 0 {
		return contextEnvelope{}
	}
	if len(msgs) == 1 {
		return contextEnvelope{current: currentMarker + "\n" + jsonLine(msgs[0])}
	}

	rest := msgs[:len(msgs)-1]
	var prefix string
	if rest[0].Role == "system" {
		prefix = rest[0].text()
		rest = rest[1:]
	}

	var history strings.Builder
	for i, m := range rest {
		if i > 0 {
			history.WriteByte('\n')
		}
		history.WriteString(jsonLine(m))
	}

	return contextEnvelope{
		prefix:  prefix,
		history: history.String(),
		current: currentMarker + "\n" + jsonLine(msgs[len(msgs)-1]),
	}
}

// render reproduces the single-message body the web endpoint accepts.
func (e contextEnvelope) render() string {
	var parts []string
	if s := strings.TrimSpace(e.prefix); s != "" {
		parts = append(parts, s)
	}
	if h := strings.TrimSpace(e.history); h != "" {
		parts = append(parts, historyMarker+"\n"+h)
	}
	if c := strings.TrimSpace(e.current); c != "" {
		parts = append(parts, c)
	}
	return strings.Join(parts, "\n\n")
}

// historyLines returns the history block as individual JSONL lines.
func (e contextEnvelope) historyLines() []string {
	if e.history == "" {
		return nil
	}
	return strings.Split(e.history, "\n")
}

// jsonLine renders one message as a JSONL line.
func jsonLine(m chatMessage) string {
	b, err := json.Marshal(map[string]string{"role": m.Role, "content": m.text()})
	if err != nil {
		return ""
	}
	return string(b)
}

// --- prefix cache ---------------------------------------------------------

// prefixEntry is one cached upload: the file descriptor plus the prefix of the
// history it contains, so a later turn can tell whether its history still starts
// with those lines.
type prefixEntry struct {
	file        map[string]any
	prefixLines int
	prefixHash  string
	createdAt   time.Time
}

// prefixCache is a small TTL + LRU cache of uploaded history attachments. Entries
// expire from creation rather than last use: an expired file_id does not error
// upstream — the model simply answers without it — so reuse has to be bounded by
// the document's own lifetime.
type prefixCache struct {
	mu      sync.Mutex
	entries map[string]*prefixEntry
	order   []string
	ttl     time.Duration
	max     int
}

func newPrefixCache(ttl time.Duration, max int) *prefixCache {
	if max < 1 {
		max = 1
	}
	return &prefixCache{entries: map[string]*prefixEntry{}, ttl: ttl, max: max}
}

func (c *prefixCache) get(key string) *prefixEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil
	}
	if c.ttl > 0 && time.Since(e.createdAt) > c.ttl {
		delete(c.entries, key)
		c.removeOrder(key)
		return nil
	}
	// Touch for LRU: move to the back of the order slice.
	c.removeOrder(key)
	c.order = append(c.order, key)
	return e
}

func (c *prefixCache) set(key string, e *prefixEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.createdAt = time.Now()
	c.entries[key] = e
	c.removeOrder(key)
	c.order = append(c.order, key)
	for len(c.order) > c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
}

func (c *prefixCache) removeOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// --- keys and matching ----------------------------------------------------

// canonicalHistoryHash hashes JSONL lines in their canonical form.
func canonicalHistoryHash(lines []string) string {
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// contextPrefixKey identifies a session for attachment reuse. It is derived from
// what stays constant across turns: the model, the system prompt, and the first
// history line. Two unrelated sessions could collide, but prefixMatches verifies
// the actual lines, so a collision only costs one redundant bake.
func contextPrefixKey(model, system, firstHistoryLine string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		model,
		system,
		firstHistoryLine,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// prefixMatches reports whether this turn's history still starts with the lines
// that were baked into the cached document.
func prefixMatches(history string, e *prefixEntry) bool {
	if e == nil || e.prefixLines <= 0 || e.prefixHash == "" {
		return false
	}
	lines := strings.Split(history, "\n")
	if len(lines) < e.prefixLines {
		return false
	}
	return canonicalHistoryHash(lines[:e.prefixLines]) == e.prefixHash
}

// firstHistoryLine returns the first JSONL line, used as part of the cache key.
func firstHistoryLine(history string) string {
	if history == "" {
		return ""
	}
	if i := strings.IndexByte(history, '\n'); i >= 0 {
		return history[:i]
	}
	return history
}

// tailLines returns the history lines after the first n, joined back together.
func tailLines(history string, n int) string {
	if n <= 0 || history == "" {
		return history
	}
	lines := strings.Split(history, "\n")
	if n >= len(lines) {
		return ""
	}
	return strings.Join(lines[n:], "\n")
}

// --- prompts --------------------------------------------------------------

// buildAttachmentNotice tells the model that the earlier history is in an
// attachment and that the inline JSONL continues it. Without this the model sees
// a history that appears to start mid-conversation.
func buildAttachmentNotice(attachmentName string, prefixLines int) string {
	return strings.Join([]string{
		"# Agent context attachment",
		fmt.Sprintf("The EARLIER part of the conversation history (the first %d JSONL messages) is attached as %s. "+
			"The \"%s\" section below CONTINUES it verbatim; together they are the complete history.",
			prefixLines, attachmentName, historyMarker),
		"The system instructions and current message are complete inline. Read the attachment as the authoritative earlier history before acting.",
		"Continue from the latest state; do not restart the task.",
	}, "\n")
}

// renderExternalized builds the inline body for an externalized turn. tail is the
// history not present in the attachment ("" right after a bake).
//
// The system prompt is truncated when it alone would blow the budget: on this
// path the history is in the attachment, but a large system prompt (an agent
// client's is tens of KB) still travels inline. Keeping its head and tail
// preserves the instructions and the most recent additions.
func renderExternalized(env contextEnvelope, attachmentName string, prefixLines int, tail string) string {
	notice := buildAttachmentNotice(attachmentName, prefixLines)
	historySection := historyMarker + "\n" + buildHistoryTailLine(attachmentName, prefixLines)
	if tail != "" {
		historySection += "\n" + tail
	}
	current := strings.TrimSpace(env.current)

	// Fixed costs that must survive: the notice, the history section, the current
	// message, and the attachment descriptor that will be added to message.files.
	fixed := len(notice) + len(historySection) + len(current) + descriptorBudgetBytes + 64
	prefix := strings.TrimSpace(env.prefix)
	if budget := defaultUpstreamBudgetBytes - fixed; len(prefix) > budget {
		if budget < 0 {
			budget = 0
		}
		prefix = truncateMiddle(prefix, budget)
	}

	var parts []string
	parts = append(parts, notice)
	if prefix != "" {
		parts = append(parts, prefix)
	}
	parts = append(parts, historySection)
	if current != "" {
		parts = append(parts, current)
	}
	return strings.Join(parts, "\n\n")
}

// descriptorBudgetBytes reserves room for the message.files descriptor, which
// travels inside the request body alongside the inline text.
const descriptorBudgetBytes = 2048

// fitsBudget reports whether a rendered inline body plus its attachment
// descriptors stays under the upstream ceiling.
func fitsBudget(inline string, descriptors int) bool {
	return len(inline)+descriptors*descriptorBudgetBytes <= defaultUpstreamBudgetBytes
}

func buildHistoryTailLine(attachmentName string, prefixLines int) string {
	return fmt.Sprintf("[earlier history: the first %d JSONL messages are in the attachment %s; "+
		"the JSONL below continues from message %d]", prefixLines, attachmentName, prefixLines+1)
}

// attachmentNameOf recovers the display name from a cached descriptor.
func attachmentNameOf(file map[string]any) string {
	if n, ok := file["name"].(string); ok && n != "" {
		return n
	}
	if f, ok := file["file"].(map[string]any); ok {
		if n, ok := f["filename"].(string); ok && n != "" {
			return n
		}
	}
	return attachmentNamePrefix + "0.txt"
}

// --- parse breaker --------------------------------------------------------

// parseBreaker stops hammering the parse endpoint once the WAF has challenged it
// several times in a row. It is not evasion — it is backing off from a WAF that
// counts parse calls per egress IP.
type parseBreaker struct {
	mu        sync.Mutex
	strikes   int
	openUntil time.Time
}

const (
	parseBreakerStrikes = 3
	parseBreakerOpen    = 5 * time.Minute
)

func (b *parseBreaker) open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().Before(b.openUntil)
}

// note records the outcome of an upload/parse attempt. Only a WAF challenge
// counts as a strike; other failures are transient and do not trip the breaker.
func (b *parseBreaker) note(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil {
		b.strikes = 0
		b.openUntil = time.Time{}
		return
	}
	if !isWAFError(err) {
		return
	}
	b.strikes++
	if b.strikes >= parseBreakerStrikes {
		b.openUntil = time.Now().Add(parseBreakerOpen)
		log.Printf("attach: parse WAF challenged %d times, pausing uploads for %s", b.strikes, parseBreakerOpen)
	}
}

func isWAFError(err error) bool {
	return err != nil && strings.Contains(err.Error(), errWAFChallenge.Error())
}

// --- orchestration --------------------------------------------------------

// externalizeResult is the outcome of preparing an upstream body.
type externalizeResult struct {
	// content is the inline message text.
	content string
	// files is the message.files list (nil when nothing was attached).
	files []any
	// mode records which path was taken, for logging.
	mode string
}

// externalizeContext prepares the upstream message body. Small conversations are
// returned inline unchanged; large ones are externalized into a document
// attachment, reusing a previous upload when this turn's history still starts
// with it. Compaction is the last resort when uploads are unavailable.
func (s *server) externalizeContext(ctx context.Context, acct *account, msgs []chatMessage, model string) externalizeResult {
	// Below the ceiling the conversation is sent verbatim, exactly as before
	// externalization existed — no marker rewriting, no attachment.
	inline := flattenMessages(msgs)
	if len(inline) <= defaultUpstreamBudgetBytes {
		return externalizeResult{content: inline, mode: "inline"}
	}

	env := buildEnvelope(msgs)

	key := contextPrefixKey(model, env.prefix, firstHistoryLine(env.history))

	// Reuse: this turn's history may still start with the cached prefix.
	if e := s.prefix.get(key); prefixMatches(env.history, e) {
		tail := tailLines(env.history, e.prefixLines)
		candidate := renderExternalized(env, attachmentNameOf(e.file), e.prefixLines, tail)
		if fitsBudget(candidate, 1) {
			log.Printf("attach: reuse %s (%d prefix lines, %d inline lines, %d bytes)",
				attachmentNameOf(e.file), e.prefixLines, countLines(tail), len(candidate))
			return externalizeResult{content: candidate, files: []any{e.file}, mode: "reuse"}
		}
		// The tail no longer fits; fall through and bake the whole history again.
	}

	// Bake: upload this turn's full history as a document.
	if env.history != "" && !s.breaker.open() {
		name := fmt.Sprintf("%s%d.txt", attachmentNamePrefix, time.Now().UnixMilli())
		file, err := uploadAgentContext(ctx, s.client, acct, env.history, name)
		s.breaker.note(err)
		if err == nil {
			lines := len(env.historyLines())
			s.prefix.set(key, &prefixEntry{
				file:        file,
				prefixLines: lines,
				prefixHash:  canonicalHistoryHash(env.historyLines()),
			})
			candidate := renderExternalized(env, name, lines, "")
			if fitsBudget(candidate, 1) {
				log.Printf("attach: baked %s (%d history lines, %d bytes inline)",
					name, lines, len(candidate))
				return externalizeResult{content: candidate, files: []any{file}, mode: "bake"}
			}
			log.Printf("attach: baked document still over budget (%d bytes), compacting", len(candidate))
		} else {
			log.Printf("attach: upload failed, falling back to compaction: %v", err)
		}
	}

	// Fallback: compaction (lossy, but never an empty reply).
	compacted, dropped := compactMessages(msgs, defaultUpstreamBudgetBytes)
	log.Printf("attach: compacted %d -> %d messages (%d -> %d bytes)",
		len(msgs), len(compacted), len(inline), len(flattenMessages(compacted)))
	_ = dropped
	return externalizeResult{content: flattenMessages(compacted), mode: "compact"}
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
