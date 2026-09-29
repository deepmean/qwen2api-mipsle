package main

import "testing"

// Reference values produced by running the upstream JavaScript directly
// (ref/fingerprint.js + ref/cookie-generator.js) under Node.
const (
	refEmail       = "test@example.com"
	refFingerprint = "973dfe463ec85785f5f9^websdk-2.3.15d^1765348410850^91^1|15^zh-TW^-480^16705151|12791^1920|1080|283|1080|158|0|1920|1080|1920|922|0|0^5^Win32^10^ANGLE (NVIDIA, NVIDIA GeForce RTX 3080 Direct3D11 vs_5_0 ps_5_0, D3D11)|Google Inc. (NVIDIA)^30|30^0^28^5|956755675^764615716^3868730536^1^0^1^0^P^0^0^0^416^Google Inc.^8^-1|0|0|0|0^2661703098^11^1765348410850^1317092247^0^69"

	refEncHello = "GK3qjO0kGH01oitqGjPGt47tLD"
	refEncAbc   = "eKeqwoD"
	refEncFull  = "mqUxuDmxIxpxnDj20Dwx0xWqoL0fxGHDy2DYK0CYDLxn4GQDU7DqeqK4Y=xeIhwpq8DGowiiqApqDs9DxiNDA24DaDU4eLsKExhGiQ9A2nPxmgKBbYT_f1O208df6OrgCnuOueXbnUjk5Y7DbaDmKDUOGdg7eDxxGTDCeDQxirDD4DAlYDFxibDimv=4DdH9OAwzOmQDGrDlKDRrxqH4GWTxDFH8DkH48h2FxDnODAniupD7yoDlPK=dnpDDvuidmEG8QRDm_br/SLDCKDjaLv5bxQ8_fA=R=YZfWXG7mk495K1hh/eQBtr0KeGd2u4i045BdKD4gAz=nYDD358eweeK4U2OUdhtcx4dGP2AKrKYHDxYAqhnLxlrmpw1nwsee_Gh67DWS55iD54hxYD"
)

func TestCustomEncodeMatchesUpstream(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello^world^test", refEncHello},
		{"abc", refEncAbc},
		{refFingerprint, refEncFull},
	}
	for _, tc := range cases {
		got := customEncode(tc.in, true)
		if got != tc.want {
			t.Errorf("customEncode(%q)\n got %q\nwant %q", truncate(tc.in), got, tc.want)
		}
	}
}

func truncate(s string) string {
	if len(s) > 30 {
		return s[:30] + "..."
	}
	return s
}

func TestDeterministicFingerprintMatchesUpstream(t *testing.T) {
	fp := generateDeterministicFingerprint(refEmail)
	got := join(fp.raw, "^")
	if got != refFingerprint {
		// Field-by-field diff makes a mismatch actionable.
		want := split(refFingerprint, "^")
		for i := 0; i < len(want) || i < len(fp.raw); i++ {
			var g, w string
			if i < len(fp.raw) {
				g = fp.raw[i]
			}
			if i < len(want) {
				w = want[i]
			}
			if g != w {
				t.Errorf("field %d: got %q want %q", i, g, w)
			}
		}
		t.Fatalf("fingerprint mismatch")
	}
}

func TestFingerprintIsStable(t *testing.T) {
	a := join(generateDeterministicFingerprint("a@b.com").raw, "^")
	b := join(generateDeterministicFingerprint("a@b.com").raw, "^")
	if a != b {
		t.Fatal("same email must yield the same identity")
	}
	c := join(generateDeterministicFingerprint("c@d.com").raw, "^")
	if a == c {
		t.Fatal("different emails must yield different identities")
	}
}

func TestGenerateCookiesShape(t *testing.T) {
	fp := generateDeterministicFingerprint(refEmail)
	c := fp.generateCookies()
	if len(c.Itna) < 10 || c.Itna[:2] != "1-" {
		t.Fatalf("ssxmod_itna malformed: %q", c.Itna)
	}
	if len(c.Itna2) < 5 || c.Itna2[:2] != "1-" {
		t.Fatalf("ssxmod_itna2 malformed: %q", c.Itna2)
	}
	// itna2 is the 18-field skeleton.
	if n := len(split(c.RawData, "^")); n != 37 {
		t.Fatalf("ssxmod_itna raw has %d fields, want 37", n)
	}
}

// small helpers so the test does not pull in strings for one call
func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

func split(s, sep string) []string {
	var out []string
	start := 0
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			out = append(out, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	return append(out, s[start:])
}
