package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func TestTimezoneHeaderMatchesFingerprint(t *testing.T) {
	// zh-CN accounts carry offset -480 in the fingerprint, which upstream renders
	// as UTC+08 (the sign is inverted relative to the raw offset).
	fp := generateDeterministicFingerprint("a@b.com")
	got := timezoneHeader(fp)
	if !strings.HasPrefix(got, "UTC+") && !strings.HasPrefix(got, "UTC-") {
		t.Fatalf("timezone = %q, want a UTC+HH / UTC-HH form", got)
	}
	if len(got) != len("UTC+08") {
		t.Fatalf("timezone = %q, want UTC±HH", got)
	}
	if timezoneHeader(nil) != "UTC+08" {
		t.Fatalf("nil fingerprint should default to UTC+08, got %q", timezoneHeader(nil))
	}
}

func TestBuildHeadersCarriesRequiredProfile(t *testing.T) {
	acct := &account{Email: "user@example.com", fp: generateDeterministicFingerprint("user@example.com")}
	h := buildHeaders(acct, "jwt-token", "application/json", "/c/abc")

	// Fields the WAF keys on: every one must be present and non-empty.
	for _, k := range []string{
		"user-agent", "sec-ch-ua", "sec-ch-ua-platform", "sec-ch-ua-mobile",
		"accept-language", "sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site",
		"timezone", "source", "version", "origin", "referer", "cookie", "x-request-id",
	} {
		if h[k] == "" {
			t.Errorf("header %q is missing", k)
		}
	}

	// The cookie must carry the token plus both ssxmod values.
	c := h["cookie"]
	for _, want := range []string{"token=jwt-token", "ssxmod_itna=1-", "ssxmod_itna2=1-"} {
		if !strings.Contains(c, want) {
			t.Errorf("cookie missing %q: %s", want, c)
		}
	}

	// sec-ch-ua major version must agree with the UA, or the profile is incoherent.
	uaMajor := ""
	if i := strings.Index(h["user-agent"], "Chrome/"); i >= 0 {
		uaMajor = h["user-agent"][i+len("Chrome/"):]
		if j := strings.IndexByte(uaMajor, '.'); j >= 0 {
			uaMajor = uaMajor[:j]
		}
	}
	if uaMajor == "" {
		t.Fatal("user-agent carries no Chrome version")
	}
	if !strings.Contains(h["sec-ch-ua"], `v="`+uaMajor+`"`) {
		t.Errorf("sec-ch-ua %q disagrees with UA major %q", h["sec-ch-ua"], uaMajor)
	}

	// Referer and origin must be built from the configured origin.
	if !strings.HasSuffix(h["referer"], "/c/abc") {
		t.Errorf("referer = %q", h["referer"])
	}
	if !strings.HasPrefix(h["origin"], "https://") {
		t.Errorf("origin = %q", h["origin"])
	}
}

func TestBuildHeadersSendsAcceptedFrontendVersion(t *testing.T) {
	acct := &account{Email: "user@example.com", fp: generateDeterministicFingerprint("user@example.com")}
	h := buildHeaders(acct, "jwt", "application/json", "/")
	if h["version"] != frontendVersion {
		t.Fatalf("version header = %q, want %q", h["version"], frontendVersion)
	}
	// The pin is deliberate and must not track the live FE build: sending
	// 0.3.12 (the current build) gets /api/v2/chats/new answered
	// `{"success":false,"data":{"code":"Unauthorized"}}`, because our ssxmod
	// fingerprint template is the 0.2.81 profile and the two must agree.
	// Bumping this requires moving the fingerprint template in ssxmod.go too.
	if frontendVersion != "0.2.81" {
		t.Fatalf("frontendVersion = %q; bumping it without the matching ssxmod template gets Unauthorized", frontendVersion)
	}
}

func TestPickChromeVersionIsStable(t *testing.T) {
	fp := generateDeterministicFingerprint("stable@example.com")
	first := pickChromeVersion(fp)
	for i := 0; i < 20; i++ {
		if pickChromeVersion(fp) != first {
			t.Fatal("Chrome version must be stable for one account")
		}
	}
	if pickChromeVersion(nil) != chromeVersions[0] {
		t.Fatal("nil fingerprint should use the first version")
	}
}

func TestParseAccountLine(t *testing.T) {
	// The password may contain ':' — only the first colon separates the fields.
	accounts, err := parseAccounts(strings.NewReader("a@b.com:pa:ss\n\n# comment\nc@d.com:pw2\n"))
	if err != nil {
		t.Fatalf("parseAccounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("got %d accounts, want 2", len(accounts))
	}
	if accounts[0].Email != "a@b.com" || accounts[0].Pass != "pa:ss" {
		t.Errorf("first account = %q / %q", accounts[0].Email, accounts[0].Pass)
	}
	if accounts[1].Email != "c@d.com" || accounts[1].Pass != "pw2" {
		t.Errorf("second account = %q / %q", accounts[1].Email, accounts[1].Pass)
	}
	if accounts[0].fp == nil {
		t.Error("account should have a fingerprint")
	}
}

func TestJWTExpiry(t *testing.T) {
	// {"exp":2000000000} base64url-encoded, unsigned — only the claim is read.
	token := "eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjIwMDAwMDAwMDB9.sig"
	if got := jwtExpiry(token); got != 2000000000 {
		t.Fatalf("jwtExpiry = %d, want 2000000000", got)
	}
	if got := jwtExpiry("not-a-jwt"); got != 0 {
		t.Fatalf("malformed token should yield 0, got %d", got)
	}
}

func TestUpstreamFailureShapes(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"error":{"message":"rate limited"}}`, true},
		{`{"error":{"code":"invalid_input"}}`, true},
		{`{"code":"RateLimited","message":"slow down"}`, true},
		{`{"status":"failed","message":"boom"}`, true},
		{`{"choices":[{"delta":{"content":"hi"}}]}`, false},
		{`{"usage":{"input_tokens":1}}`, false},
	}
	for _, tc := range cases {
		var root map[string]any
		if err := jsonUnmarshal(tc.body, &root); err != nil {
			t.Fatalf("bad fixture %q: %v", tc.body, err)
		}
		if _, ok := upstreamFailure(root); ok != tc.want {
			t.Errorf("upstreamFailure(%s) = %v, want %v", tc.body, ok, tc.want)
		}
	}
}

// The model catalogue advertises a -image variant only for models whose upstream
// metadata declares the t2i capability, so a client can discover it.
func TestHasChatType(t *testing.T) {
	types := []string{"t2t", "t2v", "t2i", "image_edit", "search"}
	if !hasChatType(types, "t2i") {
		t.Fatal("t2i should be detected")
	}
	if hasChatType(types, "t2i-other") {
		t.Fatal("a different capability must not match")
	}
	if hasChatType(nil, "t2i") {
		t.Fatal("nil capability list must not match")
	}
	// Upstream has been observed padding values with whitespace.
	if !hasChatType([]string{" t2i "}, "t2i") {
		t.Fatal("whitespace-padded capability should match")
	}
}
