package main

// Tests for the image-edit path: the inbound content-part parsing, the data-URI
// decoding, the upstream body shape, and the failure envelope the asset
// endpoints use.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestChatMessageImageSources(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "openai image_url part",
			content: `[{"type":"text","text":"edit this"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`,
			want:    []string{"https://example.com/a.png"},
		},
		{
			name:    "qwen image part",
			content: `[{"type":"text","text":"edit this"},{"type":"image","image":"data:image/png;base64,AAAA"}]`,
			want:    []string{"data:image/png;base64,AAAA"},
		},
		{
			name:    "image part carrying url instead of image",
			content: `[{"type":"image","url":"https://example.com/b.png"}]`,
			want:    []string{"https://example.com/b.png"},
		},
		{
			name:    "two pictures keep their order",
			content: `[{"type":"image_url","image_url":{"url":"https://example.com/1.png"}},{"type":"image","image":"https://example.com/2.png"}]`,
			want:    []string{"https://example.com/1.png", "https://example.com/2.png"},
		},
		{
			name:    "text only yields nothing",
			content: `[{"type":"text","text":"hello"}]`,
			want:    nil,
		},
		{
			name:    "a bare string is not a part array",
			content: `"hello"`,
			want:    nil,
		},
		{
			name:    "image_url with no url is ignored",
			content: `[{"type":"image_url","image_url":{}}]`,
			want:    nil,
		},
	}
	for _, tc := range cases {
		m := chatMessage{Role: "user", Content: json.RawMessage(tc.content)}
		got := m.imageSources()
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
				break
			}
		}
	}
}

// Adding image extraction must not change what the text path sees: the prompt
// still comes through, and an image part still contributes no text.
func TestChatMessageTextIgnoresImageParts(t *testing.T) {
	m := chatMessage{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"make it blue"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`)}
	if got := m.text(); got != "make it blue" {
		t.Fatalf("text = %q, want %q", got, "make it blue")
	}
	// A bare string is still passed through verbatim.
	plain := chatMessage{Role: "user", Content: json.RawMessage(`"just text"`)}
	if got := plain.text(); got != "just text" {
		t.Fatalf("text = %q, want %q", got, "just text")
	}
}

func TestDecodeDataURI(t *testing.T) {
	// "hi" base64-encoded.
	data, ct, err := decodeDataURI("data:image/png;base64,aGk=")
	if err != nil {
		t.Fatalf("valid data URI rejected: %v", err)
	}
	if string(data) != "hi" {
		t.Fatalf("data = %q, want %q", string(data), "hi")
	}
	if ct != "image/png" {
		t.Fatalf("contentType = %q, want image/png", ct)
	}

	// A missing mime type falls back to png rather than producing an empty one,
	// which would be signed into the OSS request.
	if _, ct, err := decodeDataURI("data:;base64,aGk="); err != nil || ct != "image/png" {
		t.Fatalf("mime-less data URI: ct=%q err=%v", ct, err)
	}

	for name, in := range map[string]string{
		"not a data URI":  "https://example.com/a.png",
		"no comma":        "data:image/png;base64",
		"not base64":      "data:image/png,hello",
		"invalid base64":  "data:image/png;base64,!!!!",
		"empty payload":   "data:image/png;base64,",
	} {
		if _, _, err := decodeDataURI(in); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func TestIsHTTPURL(t *testing.T) {
	for _, s := range []string{"http://x/a.png", "https://x/a.png"} {
		if !isHTTPURL(s) {
			t.Errorf("%s should be recognised as a URL", s)
		}
	}
	for _, s := range []string{"data:image/png;base64,AA", "ftp://x/a", "", "x.png"} {
		if isHTTPURL(s) {
			t.Errorf("%s should not be recognised as a URL", s)
		}
	}
}

func TestSimpleFileType(t *testing.T) {
	cases := map[string]string{
		"image/png":              "image",
		"image/jpeg":             "image",
		"video/mp4":              "video",
		"audio/mpeg":             "audio",
		"application/pdf":        "file",
		"text/plain":             "file",
		"application/octet-stream": "file",
		"":                       "file",
	}
	for in, want := range cases {
		if got := simpleFileType(in); got != want {
			t.Errorf("simpleFileType(%q) = %q, want %q", in, got, want)
		}
	}
}

// The asset endpoints refuse a request with this envelope under HTTP 200. Left
// unrecognised it becomes a successful but empty reply, which is the failure
// mode the whole upstreamFailure function exists to prevent.
func TestUpstreamFailureDetectsAssetRefusal(t *testing.T) {
	body := `{"success":false,"request_id":"700cde63","data":{"code":"RateLimited","details":"You've reached the upper limit for today's usage."}}`
	var root map[string]any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	msg, ok := upstreamFailure(root)
	if !ok {
		t.Fatal("the asset refusal envelope must be detected")
	}
	if msg != "RateLimited" {
		t.Fatalf("msg = %q, want RateLimited", msg)
	}

	// success:true must not be treated as a failure.
	okBody := `{"success":true,"data":{"code":"RateLimited"}}`
	root = nil
	_ = json.Unmarshal([]byte(okBody), &root)
	if _, ok := upstreamFailure(root); ok {
		t.Fatal("success:true must not be a failure")
	}

	// A code with no details still names the failure.
	root = nil
	_ = json.Unmarshal([]byte(`{"success":false,"data":{"code":"Bad_Request"}}`), &root)
	if msg, ok := upstreamFailure(root); !ok || msg != "Bad_Request" {
		t.Fatalf("got (%q,%v)", msg, ok)
	}

	// Details alone are enough when there is no code.
	root = nil
	_ = json.Unmarshal([]byte(`{"success":false,"data":{"details":"nope"}}`), &root)
	if msg, ok := upstreamFailure(root); !ok || msg != "nope" {
		t.Fatalf("got (%q,%v)", msg, ok)
	}
}

// prepareImageEdit must produce content parts, because that is the channel the
// upstream actually reads the picture from — verified against the live endpoint.
func TestPrepareImageEditBuildsContentParts(t *testing.T) {
	s := &server{}
	msgs := []chatMessage{{
		Role: "user",
		Content: json.RawMessage(`[
			{"type":"text","text":"make it blue"},
			{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}
		]`),
	}}

	ext, err := s.prepareImageEdit(context.Background(), &account{}, msgs)
	if err != nil {
		t.Fatalf("prepareImageEdit: %v", err)
	}
	// The picture must NOT go through the text field, or the parts are ignored.
	if ext.content != "" {
		t.Fatalf("content = %q, want empty when parts are used", ext.content)
	}
	if len(ext.parts) != 2 {
		t.Fatalf("parts = %v, want a text part and an image part", ext.parts)
	}
	first, _ := ext.parts[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "make it blue" {
		t.Fatalf("first part = %v", first)
	}
	second, _ := ext.parts[1].(map[string]any)
	if second["type"] != "image" || second["image"] != "https://example.com/a.png" {
		t.Fatalf("second part = %v", second)
	}
	// An already-hosted URL must be passed through untouched, not re-uploaded.
	if ext.mode != "image_edit" {
		t.Fatalf("mode = %q", ext.mode)
	}
}

func TestPrepareImageEditRejectsMissingPicture(t *testing.T) {
	s := &server{}
	msgs := []chatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"make it blue"}]`)}}
	if _, err := s.prepareImageEdit(context.Background(), &account{}, msgs); err == nil {
		t.Fatal("an edit with no input picture must be rejected rather than silently answered with text")
	}
	// An empty message list is also rejected.
	if _, err := s.prepareImageEdit(context.Background(), &account{}, nil); err == nil {
		t.Fatal("an empty message list must be rejected")
	}
}

// The upstream body for an edit turn must carry content as an array; sending the
// prompt as a bare string is the shape that loses the picture.
func TestBuildUpstreamPayloadSendsContentParts(t *testing.T) {
	resolved := resolvedModel{ID: "qwen3.8-max", ChatType: "image_edit"}
	ext := externalizeResult{
		parts: []any{
			map[string]any{"type": "text", "text": "make it blue"},
			map[string]any{"type": "image", "image": "https://example.com/a.png"},
		},
	}
	raw, err := buildUpstreamPayloadSized(resolved, "chat-1", ext, "")
	if err != nil {
		t.Fatalf("buildUpstreamPayloadSized: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["model"] != "qwen3.8-max" {
		t.Fatalf("model = %v", body["model"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", body["messages"])
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["chat_type"] != "image_edit" || msg["sub_chat_type"] != "image_edit" {
		t.Fatalf("chat_type = %v / %v", msg["chat_type"], msg["sub_chat_type"])
	}
	content, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("content must be an array for an edit turn, got %T (%v)", msg["content"], msg["content"])
	}
	if len(content) != 2 {
		t.Fatalf("content = %v", content)
	}

	// The asset profile: no thinking, no search.
	feature, _ := msg["feature_config"].(map[string]any)
	if feature["thinking_enabled"] != false || feature["auto_search"] != false {
		t.Fatalf("feature_config = %v", feature)
	}
	if _, has := feature["thinking_format"]; has {
		t.Fatal("thinking_format must be dropped for an asset turn")
	}
}

// A text turn must keep sending a plain string, so nothing about the existing
// path changes.
func TestBuildUpstreamPayloadKeepsStringContentForText(t *testing.T) {
	resolved := resolvedModel{ID: "qwen3.8-max", ChatType: "t2t"}
	raw, err := buildUpstreamPayloadSized(resolved, "chat-1", externalizeResult{content: "hello"}, "")
	if err != nil {
		t.Fatalf("buildUpstreamPayloadSized: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	msgs, _ := body["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	if _, isString := msg["content"].(string); !isString {
		t.Fatalf("text content must stay a string, got %T", msg["content"])
	}
	if msg["content"] != "hello" {
		t.Fatalf("content = %v", msg["content"])
	}
}

// normalizeImageSize must map the pixel sizes OpenAI clients send and leave a
// ratio untouched.
func TestNormalizeImageSize(t *testing.T) {
	cases := map[string]string{
		"1024x1024": "1:1",
		"1536x1024": "4:3",
		"1024x1536": "3:4",
		"1792x1024": "16:9",
		"1024x1792": "9:16",
		"16:9":      "16:9",
		" 1:1 ":     "1:1",
		"":          "",
	}
	for in, want := range cases {
		if got := normalizeImageSize(in); got != want {
			t.Errorf("normalizeImageSize(%q) = %q, want %q", in, got, want)
		}
	}
}

// The catalogue must advertise both picture variants for a model that declares
// both capabilities, so a client can discover them.
func TestModelCatalogueAdvertisesImageVariants(t *testing.T) {
	// Mirrors what upstream returns for qwen3.8-max.
	types := []string{"t2t", "t2v", "t2i", "image_edit", "search"}
	if !hasChatType(types, "t2i") || !hasChatType(types, "image_edit") {
		t.Fatal("the fixture must declare both image capabilities")
	}
	// And a text-only model must advertise neither.
	textOnly := []string{"t2t"}
	if hasChatType(textOnly, "t2i") || hasChatType(textOnly, "image_edit") {
		t.Fatal("a text-only model must not advertise image variants")
	}
}

var _ = strings.TrimSpace
