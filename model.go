package main

// Inbound model-name normalisation.
//
// Ported from Qwen2API src/utils/model-map.js (stripBracketSuffix, resolveModel)
// and src/utils/chat-helpers.js (splitModelSuffix, isChatType).
//
// Clients decorate the model name with information the upstream does not accept:
//
//   - A trailing bracket group, e.g. `qwen3.8-max[1M]`. Claude Code appends
//     `[1M]` to signal a 1M-token context window. Passing it through unchanged
//     does NOT produce an error — Qwen answers HTTP 200 with an empty stream,
//     so the client sees the turn end instantly with no reply.
//   - A behavioural suffix, e.g. `qwen3.8-max-thinking`. These are meaningful:
//     the suffix selects the chat type and whether thinking is enabled, but the
//     name sent upstream must be the bare model id.

import (
	"strings"
)

// modelSuffixes is ordered longest-first, matching upstream MODEL_SUFFIXES;
// splitModelSuffix takes the first hit.
var modelSuffixes = []string{
	"-thinking-search",
	"-image-edit",
	"-deep-research",
	"-thinking",
	"-search",
	"-video",
	"-image",
}

// stripBracketSuffix removes trailing `[...]` groups, repeated and optionally
// space-separated: `foo[1m][x]` and `foo [1m]` both become `foo`.
//
// This mirrors the upstream regex `(\s*\[[^\]]*\])+$` without pulling in the
// regexp package.
func stripBracketSuffix(name string) string {
	s := strings.TrimSpace(name)
	for strings.HasSuffix(s, "]") {
		i := strings.LastIndex(s, "[")
		if i < 0 {
			break
		}
		s = strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// splitModelSuffix separates a behavioural suffix from the base model name.
// The returned suffix includes its leading dash, or is empty when none matched.
func splitModelSuffix(model string) (base, suffix string) {
	for _, sfx := range modelSuffixes {
		if strings.HasSuffix(model, sfx) {
			return model[:len(model)-len(sfx)], sfx
		}
	}
	return model, ""
}

// chatTypeForSuffix maps a behavioural suffix onto the upstream chat_type.
//
// `-image` selects t2i (text-to-image). The upstream answers it on the ordinary
// completions stream: the generated picture arrives as a URL in
// `choices[0].delta.content` with `phase: "image_gen"`, so it needs no separate
// transport — see the note on deltaNormalizer.
func chatTypeForSuffix(suffix string) string {
	switch suffix {
	case "-search", "-thinking-search":
		return "search"
	case "-image":
		return "t2i"
	default:
		return "t2t"
	}
}

// unsupportedChatType reports whether a suffix selects a modality this port does
// not implement, so the caller can fail loudly instead of silently answering
// with text for a request it cannot serve.
//
// -image-edit needs the caller's input picture carried in message.files, which
// this build does not parse (chatMessage.text drops non-text parts), so it would
// silently edit nothing. -video needs task polling over a multi-minute window,
// which is a different lifecycle. Both stay rejected on purpose.
func unsupportedChatType(suffix string) (string, bool) {
	switch suffix {
	case "-image-edit":
		return "image editing", true
	case "-video":
		return "video generation", true
	case "-deep-research":
		return "deep research", true
	default:
		return "", false
	}
}

// resolvedModel is the outcome of normalising an inbound model name.
type resolvedModel struct {
	// ID is the name to send upstream.
	ID string
	// ChatType is the upstream chat_type ("t2t" or "search").
	ChatType string
	// Thinking reports whether reasoning should be requested.
	Thinking bool
	// Unsupported names a modality this port does not implement, if any.
	Unsupported string
}

// resolveModel normalises an inbound model name. `known` is the upstream model
// catalogue, used to map an alias or display name onto its canonical id; it may
// be nil, in which case the stripped name is used as-is.
func resolveModel(name string, known []modelInfo, enableThinking bool) resolvedModel {
	stripped := stripBracketSuffix(name)
	if stripped == "" {
		stripped = "qwen3.6-plus"
	}

	base, suffix := splitModelSuffix(stripped)
	if base == "" {
		base = stripped
	}

	out := resolvedModel{
		ID:       base,
		ChatType: chatTypeForSuffix(suffix),
		Thinking: enableThinking || suffix == "-thinking" || suffix == "-thinking-search",
	}
	if what, bad := unsupportedChatType(suffix); bad {
		out.Unsupported = what
		return out
	}

	// Prefer the canonical id from the live catalogue when the name matches.
	for _, m := range known {
		if m.ID == base || strings.EqualFold(m.ID, base) || m.Name == base {
			out.ID = m.ID
			break
		}
	}
	return out
}
