package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestOSSSignatureMatchesReference pins the Signature-V1 canonical string against
// a value computed independently (Python hmac/hashlib). Getting this wrong makes
// every upload fail with 403, and the failure surfaces only at runtime.
func TestOSSSignatureMatchesReference(t *testing.T) {
	const (
		ak     = "STS.test"
		sk     = "secretkey123"
		date   = "Mon, 02 Jan 2006 15:04:05 GMT"
		ctype  = "text/plain"
		token  = "tok123"
		bucket = "qwen-webui-prod"
		key    = "uid/file_QWEN2API_AGENT_HISTORY.txt"
		want   = "6l0i/Olml8O6A/p3sqH8mKWIihk="
	)

	canonical := "PUT\n\n" + ctype + "\n" + date + "\n" +
		"x-oss-security-token:" + token + "\n" +
		"/" + bucket + "/" + key
	mac := hmac.New(sha1.New, []byte(sk))
	mac.Write([]byte(canonical))
	got := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if got != want {
		t.Fatalf("signature = %q, want %q\ncanonical=%q", got, want, canonical)
	}
	_ = ak
}

func TestSTSResponseUnwrapsEnvelope(t *testing.T) {
	// FE 0.2.81 wraps the payload as {success,data}; older builds return it flat.
	wrapped := `{"success":true,"data":{"access_key_id":"a","access_key_secret":"b","security_token":"c","file_path":"k","bucketname":"bk","endpoint":"ep","file_id":"fid","file_url":"u"}}`
	var s stsResponse
	if err := json.Unmarshal([]byte(wrapped), &s); err != nil {
		t.Fatal(err)
	}
	u := s.unwrap()
	if u.AccessKeyID != "a" || u.FileID != "fid" || u.BucketName != "bk" {
		t.Fatalf("unwrap lost fields: %+v", u)
	}

	flat := `{"access_key_id":"a","file_id":"fid"}`
	var f stsResponse
	if err := json.Unmarshal([]byte(flat), &f); err != nil {
		t.Fatal(err)
	}
	if got := f.unwrap(); got.AccessKeyID != "a" {
		t.Fatalf("flat unwrap = %+v", got)
	}
}

func TestParseStatusOfFindsFile(t *testing.T) {
	body := map[string]any{
		"success": true,
		"data": []any{
			map[string]any{"file_id": "other", "status": "running"},
			map[string]any{"file_id": "target", "status": "Success"},
		},
	}
	if got := parseStatusOf(body, "target"); got != "success" {
		t.Fatalf("status = %q, want success", got)
	}
	if got := parseStatusOf(body, "missing"); got != "" {
		t.Fatalf("missing file status = %q, want empty", got)
	}
}

func TestLooksLikeWAFPage(t *testing.T) {
	if !looksLikeWAFPage([]byte("<!DOCTYPE html><html><meta name=\"aliyun_waf_captcha\">")) {
		t.Fatal("HTML captcha page not detected")
	}
	if looksLikeWAFPage([]byte(`{"success":true,"data":[]}`)) {
		t.Fatal("JSON body misdetected as WAF page")
	}
}

func TestBuildChatFileDescriptorShape(t *testing.T) {
	d := buildChatFileDescriptor("fid-1", "https://oss/x.txt", "QWEN2API_AGENT_HISTORY_1.txt", 4096)
	if d["type"] != "file" || d["status"] != "uploaded" || d["id"] != "fid-1" {
		t.Fatalf("descriptor top-level wrong: %+v", d)
	}
	if d["url"] != "https://oss/x.txt" || d["file_class"] != "document" || d["context"] != "full" {
		t.Fatalf("descriptor fields wrong: %+v", d)
	}
	f, _ := d["file"].(map[string]any)
	if f["id"] != "fid-1" || f["type"] != "text/plain" || f["size"] != 4096 {
		t.Fatalf("nested file wrong: %+v", f)
	}
	meta, _ := f["meta"].(map[string]any)
	pm, _ := meta["parse_meta"].(map[string]any)
	if pm["parse_status"] != "success" {
		t.Fatalf("parse_meta wrong: %+v", meta)
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: json.RawMessage(`"be terse"`)},
		{Role: "user", Content: json.RawMessage(`"one"`)},
		{Role: "assistant", Content: json.RawMessage(`"two"`)},
		{Role: "user", Content: json.RawMessage(`"three"`)},
	}
	env := buildEnvelope(msgs)
	if env.prefix != "be terse" {
		t.Fatalf("prefix = %q", env.prefix)
	}
	if env.history != `{"content":"one","role":"user"}`+"\n"+`{"content":"two","role":"assistant"}` {
		t.Fatalf("history = %q", env.history)
	}
	if !strings.Contains(env.current, `"three"`) || !strings.HasPrefix(env.current, currentMarker) {
		t.Fatalf("current = %q", env.current)
	}
	if !strings.Contains(env.render(), historyMarker) {
		t.Fatalf("render missing marker: %q", env.render())
	}
}

func TestPrefixCacheReuseAndExpiry(t *testing.T) {
	c := newPrefixCache(50*time.Millisecond, 2)
	history := "a\nb\nc"
	e := &prefixEntry{
		file:        map[string]any{"name": "f.txt"},
		prefixLines: 2,
		prefixHash:  canonicalHistoryHash([]string{"a", "b"}),
	}
	c.set("k", e)

	// Same first two lines -> match, regardless of the tail.
	if !prefixMatches(history, c.get("k")) {
		t.Fatal("expected prefix match")
	}
	// A different first line -> no match.
	if prefixMatches("x\nb\nc", c.get("k")) {
		t.Fatal("prefix matched with a changed head line")
	}
	// Fewer lines than the prefix -> no match.
	if prefixMatches("a", c.get("k")) {
		t.Fatal("prefix matched with too few lines")
	}

	time.Sleep(70 * time.Millisecond)
	if c.get("k") != nil {
		t.Fatal("entry should have expired")
	}
}

func TestPrefixCacheEvictsOldest(t *testing.T) {
	c := newPrefixCache(time.Minute, 2)
	c.set("a", &prefixEntry{prefixLines: 1, prefixHash: "x"})
	c.set("b", &prefixEntry{prefixLines: 1, prefixHash: "y"})
	c.set("c", &prefixEntry{prefixLines: 1, prefixHash: "z"})
	if c.get("a") != nil {
		t.Fatal("oldest entry should have been evicted")
	}
	if c.get("b") == nil || c.get("c") == nil {
		t.Fatal("newer entries should remain")
	}
}

func TestContextPrefixKeyIsStablePerSession(t *testing.T) {
	a := contextPrefixKey("m", "sys", "line1")
	b := contextPrefixKey("m", "sys", "line1")
	c := contextPrefixKey("m", "sys", "line2")
	if a != b {
		t.Fatal("key is not deterministic")
	}
	if a == c {
		t.Fatal("key ignored the first history line")
	}
}

func TestTailLines(t *testing.T) {
	h := "a\nb\nc\nd"
	if got := tailLines(h, 2); got != "c\nd" {
		t.Fatalf("tail = %q", got)
	}
	if got := tailLines(h, 0); got != h {
		t.Fatalf("zero prefix should keep all: %q", got)
	}
	if got := tailLines(h, 9); got != "" {
		t.Fatalf("over-long prefix should yield empty: %q", got)
	}
}

func TestParseBreakerTripsOnWAFOnly(t *testing.T) {
	var b parseBreaker
	b.note(nil)
	for i := 0; i < parseBreakerStrikes; i++ {
		b.note(errWAFChallenge)
	}
	if !b.open() {
		t.Fatal("breaker should be open after repeated WAF challenges")
	}

	// A non-WAF error must not trip the breaker.
	var b2 parseBreaker
	for i := 0; i < parseBreakerStrikes+2; i++ {
		b2.note(errNoAccounts)
	}
	if b2.open() {
		t.Fatal("breaker should ignore non-WAF errors")
	}

	// Success resets it.
	var b3 parseBreaker
	for i := 0; i < parseBreakerStrikes; i++ {
		b3.note(errWAFChallenge)
	}
	b3.note(nil)
	if b3.open() {
		t.Fatal("breaker should reset after a success")
	}
}
