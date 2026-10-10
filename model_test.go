package main

import "testing"

func TestStripBracketSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"qwen3.8-max[1M]", "qwen3.8-max"},
		{"qwen3.8-max", "qwen3.8-max"},
		{"qwen3.8-max [1m]", "qwen3.8-max"},
		{"qwen3.8-max[1m][x]", "qwen3.8-max"},
		{"  qwen3.8-max[1M]  ", "qwen3.8-max"},
		// A bracket in the middle is part of the name and must survive.
		{"weird[1m]name", "weird[1m]name"},
		{"[1M]", ""},
	}
	for _, tc := range cases {
		if got := stripBracketSuffix(tc.in); got != tc.want {
			t.Errorf("stripBracketSuffix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSplitModelSuffix(t *testing.T) {
	cases := []struct{ in, base, suffix string }{
		{"qwen3.8-max", "qwen3.8-max", ""},
		{"qwen3.8-max-thinking", "qwen3.8-max", "-thinking"},
		{"qwen3.8-max-search", "qwen3.8-max", "-search"},
		// Longest match wins: -thinking-search before -search.
		{"qwen3.8-max-thinking-search", "qwen3.8-max", "-thinking-search"},
		{"qwen3.8-max-image", "qwen3.8-max", "-image"},
	}
	for _, tc := range cases {
		base, sfx := splitModelSuffix(tc.in)
		if base != tc.base || sfx != tc.suffix {
			t.Errorf("splitModelSuffix(%q) = (%q,%q), want (%q,%q)", tc.in, base, sfx, tc.base, tc.suffix)
		}
	}
}

// The regression this whole file exists for: Claude Code appends [1M] and the
// upstream answers 200 with an empty stream when it sees the decorated name.
func TestResolveModelStripsContextMarker(t *testing.T) {
	r := resolveModel("qwen3.8-max[1M]", nil, false)
	if r.ID != "qwen3.8-max" {
		t.Fatalf("ID = %q, want qwen3.8-max", r.ID)
	}
	if r.ChatType != "t2t" {
		t.Fatalf("ChatType = %q, want t2t", r.ChatType)
	}
	if r.Thinking {
		t.Fatal("thinking must stay off unless requested")
	}
}

func TestResolveModelSuffixDrivesBehaviour(t *testing.T) {
	// -thinking enables reasoning without touching the upstream name.
	r := resolveModel("qwen3.8-max-thinking", nil, false)
	if r.ID != "qwen3.8-max" || !r.Thinking {
		t.Fatalf("got ID=%q thinking=%v", r.ID, r.Thinking)
	}

	// -search switches the chat type.
	r = resolveModel("qwen3.8-max-search", nil, false)
	if r.ID != "qwen3.8-max" || r.ChatType != "search" {
		t.Fatalf("got ID=%q chatType=%q", r.ID, r.ChatType)
	}

	// Explicit enable_thinking is honoured on a plain name.
	if r = resolveModel("qwen3.8-max", nil, true); !r.Thinking {
		t.Fatal("enable_thinking=true must turn thinking on")
	}
}

// -image is now served (t2i): upstream answers it on the ordinary stream with the
// picture URL on delta.content under phase "image_gen". The modalities that
// genuinely need machinery this port lacks stay rejected.
func TestResolveModelRejectsUnimplementedModalities(t *testing.T) {
	for _, name := range []string{"qwen3.8-max-image-edit", "qwen3.8-max-video", "qwen3.8-max-deep-research"} {
		r := resolveModel(name, nil, false)
		if r.Unsupported == "" {
			t.Errorf("%s should be reported unsupported, got ID=%q", name, r.ID)
		}
	}
}

// The image capability is selected by the -image suffix and must map to the t2i
// chat_type, with the suffix stripped from the name sent upstream.
func TestResolveModelRoutesImageSuffixToT2I(t *testing.T) {
	r := resolveModel("qwen3.8-max-image", nil, false)
	if r.Unsupported != "" {
		t.Fatalf("qwen3.8-max-image must be supported, got unsupported=%q", r.Unsupported)
	}
	if r.ID != "qwen3.8-max" {
		t.Fatalf("ID = %q, want qwen3.8-max", r.ID)
	}
	if r.ChatType != "t2i" {
		t.Fatalf("ChatType = %q, want t2i", r.ChatType)
	}
	// A generated picture is not a reasoning answer.
	if r.Thinking {
		t.Fatal("thinking must stay off for an image request")
	}
}

func TestResolveModelPrefersCatalogueID(t *testing.T) {
	known := []modelInfo{
		{ID: "qwen3.8-max", Name: "Qwen3.8 Max"},
		{ID: "qwen3.6-plus", Name: "Qwen3.6 Plus"},
	}
	// Matching by display name resolves to the canonical id.
	r := resolveModel("Qwen3.8 Max[1M]", known, false)
	if r.ID != "qwen3.8-max" {
		t.Fatalf("ID = %q, want qwen3.8-max", r.ID)
	}
	// An unknown name is passed through stripped rather than rejected.
	r = resolveModel("some-new-model[1M]", known, false)
	if r.ID != "some-new-model" {
		t.Fatalf("ID = %q, want some-new-model", r.ID)
	}
}

func TestResolveModelEmptyFallsBack(t *testing.T) {
	if r := resolveModel("", nil, false); r.ID == "" {
		t.Fatal("empty model name must fall back to a default")
	}
}
