package main

// SSXMOD cookie generation.
//
// Ported from Qwen2API src/utils/cookie-generator.js + src/utils/fingerprint.js.
// The upstream Qwen web client ships a `ssxmod_itna` / `ssxmod_itna2` cookie pair
// built by LZW-compressing a `^`-separated browser fingerprint and encoding it
// with a custom base64 alphabet. The WAF treats a missing/!malformed pair as a
// bot signal, so this must reproduce the original byte-for-byte.
//
// The fingerprint is derived deterministically from the account email
// (SHA-256 seed) so one account keeps one stable identity across restarts,
// while the random hash fields are re-randomised on every generation — exactly
// what upstream does on each page load.

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// customBase64Chars is the alphabet the Qwen web bundle uses instead of RFC 4648.
const customBase64Chars = "DGi0YA7BemWnQjCl4_bR3f8SKIF9tUz/xhr2oEOgPpac=61ZqwTudLkM5vHyNXsVJ"

// lzwCompress packs data with the variant LZW used by the bundle. Each output
// symbol carries `bits` bits (6 in practice) and is mapped through charFunc.
// Bit order is most-significant-first within each symbol, matching the JS.
func lzwCompress(data string, bits int, charFunc func(int) byte) []byte {
	if data == "" {
		return nil
	}

	dict := make(map[string]int, len(data))
	dictToCreate := make(map[string]bool)
	var w string
	enlargeIn := 2
	dictSize := 3
	numBits := 2
	result := make([]byte, 0, len(data))
	value := 0
	position := 0

	emit := func(v int) {
		value = (value << 1) | (v & 1)
		if position == bits-1 {
			position = 0
			result = append(result, charFunc(value))
			value = 0
		} else {
			position++
		}
	}

	// emitLow emits `n` low bits of code, least-significant-first.
	emitLow := func(code, n int) {
		for j := 0; j < n; j++ {
			emit(code & 1)
			code >>= 1
		}
	}

	for i := 0; i < len(data); i++ {
		c := string(data[i])

		if _, ok := dict[c]; !ok {
			dict[c] = dictSize
			dictSize++
			dictToCreate[c] = true
		}

		wc := w + c

		if _, ok := dict[wc]; ok {
			w = wc
		} else {
			if dictToCreate[w] {
				if len(w) > 0 && int(w[0]) < 256 {
					emitLow(0, numBits)
					emitLow(int(w[0]), 8)
				} else {
					emitLow(1, numBits)
					emitLow(int(w[0]), 16)
				}

				enlargeIn--
				if enlargeIn == 0 {
					enlargeIn = 1 << numBits
					numBits++
				}
				delete(dictToCreate, w)
			} else {
				emitLow(dict[w], numBits)
			}

			enlargeIn--
			if enlargeIn == 0 {
				enlargeIn = 1 << numBits
				numBits++
			}

			dict[wc] = dictSize
			dictSize++
			w = c
		}
	}

	if w != "" {
		if dictToCreate[w] {
			if len(w) > 0 && int(w[0]) < 256 {
				emitLow(0, numBits)
				emitLow(int(w[0]), 8)
			} else {
				emitLow(1, numBits)
				emitLow(int(w[0]), 16)
			}

			enlargeIn--
			if enlargeIn == 0 {
				numBits++
			}
			delete(dictToCreate, w)
		} else {
			emitLow(dict[w], numBits)
		}

		enlargeIn--
		if enlargeIn == 0 {
			// Dead store upstream too: enlargeIn is never read again.
			numBits++
		}
	}

	emitLow(2, numBits)

	for {
		value <<= 1
		if position == bits-1 {
			result = append(result, charFunc(value))
			break
		}
		position++
	}

	return result
}

// customEncode compresses and maps to the custom alphabet. urlSafe=true returns
// the raw stream (no `=` padding), which is what both cookies use.
func customEncode(data string, urlSafe bool) string {
	buf := lzwCompress(data, 6, func(i int) byte { return customBase64Chars[i] })
	if urlSafe {
		return string(buf)
	}
	switch len(buf) % 4 {
	case 1:
		return string(buf) + "==="
	case 2:
		return string(buf) + "=="
	case 3:
		return string(buf) + "="
	}
	return string(buf)
}

func randomHash() uint32 { return rand.Uint32() }

func generateDeviceID() string {
	const hexDigits = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteByte(hexDigits[rand.Intn(16)])
	}
	return b.String()
}

// --- Fingerprint template (mirrors fingerprint.js DEFAULT_TEMPLATE) ---

var defaultTemplate = []string{
	/* 0  deviceId      */ "",
	/* 1  sdkVersion    */ "websdk-2.3.15d",
	/* 2  initTimestamp */ "1765348410850",
	/* 3                */ "91",
	/* 4                */ "1|15",
	/* 5  language      */ "zh-CN",
	/* 6  timezoneOffset*/ "-480",
	/* 7  colorDepth    */ "16705151|12791",
	/* 8  screenInfo    */ "1470|956|283|797|158|0|1470|956|1470|798|0|0",
	/* 9                */ "5",
	/* 10 platform      */ "MacIntel",
	/* 11               */ "10",
	/* 12 webglRenderer */ "ANGLE (Apple, ANGLE Metal Renderer: Apple M4, Unspecified Version)|Google Inc. (Apple)",
	/* 13               */ "30|30",
	/* 14               */ "0",
	/* 15               */ "28",
	/* 16 pluginCount   */ "5",
	/* 17 canvasHash    */ "",
	/* 18 uaHash1       */ "",
	/* 19               */ "1",
	/* 20               */ "0",
	/* 21               */ "1",
	/* 22               */ "0",
	/* 23 mode          */ "P",
	/* 24               */ "0",
	/* 25               */ "0",
	/* 26               */ "0",
	/* 27               */ "416",
	/* 28 vendor        */ "Google Inc.",
	/* 29               */ "8",
	/* 30 touchInfo     */ "-1|0|0|0|0",
	/* 31 uaHash2       */ "",
	/* 32               */ "11",
	/* 33 timestamp     */ "",
	/* 34 urlHash       */ "",
	/* 35               */ "0",
	/* 36 docHash       */ "",
}

// screenPresets / platformPresets / languagePresets mirror the JS tables.
var screenPresets = map[string]string{
	"1920x1080": "1920|1080|283|1080|158|0|1920|1080|1920|922|0|0",
	"2560x1440": "2560|1440|283|1440|158|0|2560|1440|2560|1282|0|0",
	"1470x956":  "1470|956|283|797|158|0|1470|956|1470|798|0|0",
	"1440x900":  "1440|900|283|900|158|0|1440|900|1440|742|0|0",
	"1536x864":  "1536|864|283|864|158|0|1536|864|1536|706|0|0",
}

// screenKeys preserves the JS insertion order — the deterministic picker indexes
// into Object.keys(), so the order is part of the contract.
var screenKeys = []string{"1920x1080", "2560x1440", "1470x956", "1440x900", "1536x864"}

type platformPreset struct{ platform, webgl, vendor string }

var platformPresets = map[string]platformPreset{
	"macIntel": {"MacIntel", "ANGLE (Apple, ANGLE Metal Renderer: Apple M4, Unspecified Version)|Google Inc. (Apple)", "Google Inc."},
	"macM1":    {"MacIntel", "ANGLE (Apple, ANGLE Metal Renderer: Apple M1, Unspecified Version)|Google Inc. (Apple)", "Google Inc."},
	"win64":    {"Win32", "ANGLE (NVIDIA, NVIDIA GeForce RTX 3080 Direct3D11 vs_5_0 ps_5_0, D3D11)|Google Inc. (NVIDIA)", "Google Inc."},
	"linux":    {"Linux x86_64", "ANGLE (Intel, Mesa Intel(R) UHD Graphics 630, OpenGL 4.6)|Google Inc. (Intel)", "Google Inc."},
}

var platformKeys = []string{"macIntel", "macM1", "win64", "linux"}

type localePreset struct{ language, timezoneOffset string }

var languagePresets = map[string]localePreset{
	"zh-CN": {"zh-CN", "-480"},
	"zh-TW": {"zh-TW", "-480"},
	"en-US": {"en-US", "480"},
	"ja-JP": {"ja-JP", "-540"},
	"ko-KR": {"ko-KR", "-540"},
}

var localeKeys = []string{"zh-CN", "zh-TW", "en-US", "ja-JP", "ko-KR"}

// fingerprint holds the per-account identity plus the pre-rendered 37-field raw
// string (with placeholder hash slots) that generateCookies re-randomises.
type fingerprint struct {
	raw []string // 37 fields, indices 17/18/31/34/36 still placeholders
	// hash is sha256(email)[:32], used to derive the Chrome major version so
	// the User-Agent and sec-ch-ua headers stay mutually consistent.
	hash string
}

// generateDeterministicFingerprint derives a stable identity from the account
// email, matching generateDeterministicFingerprint in fingerprint.js.
func generateDeterministicFingerprint(email string) *fingerprint {
	sum := sha256.Sum256([]byte(email))
	h := hex.EncodeToString(sum[:])

	fields := make([]string, 37)
	copy(fields, defaultTemplate)

	fields[0] = h[0:20]

	plat := platformPresets[platformKeys[hexNibblePair(h, 20)%len(platformKeys)]]
	fields[10] = plat.platform
	fields[12] = plat.webgl
	fields[28] = plat.vendor

	fields[8] = screenPresets[screenKeys[hexNibblePair(h, 22)%len(screenKeys)]]

	loc := languagePresets[localeKeys[hexNibblePair(h, 24)%len(localeKeys)]]
	fields[5] = loc.language
	fields[6] = loc.timezoneOffset

	// Hash slots come from distinct slices of the digest so the whole identity
	// is reproducible (upstream replaces Math.random() the same way).
	pluginHash := hexUint(h[26:34])
	canvasHash := hexUint(h[34:42])
	uaHash1 := hexUint(h[42:50])
	uaHash2 := hexUint(h[50:58])
	urlHash := hexUint(h[58:64] + h[0:2])
	docHash := hexNibblePair(h, 62)%91 + 10

	fields[16] = defaultTemplate[16] + "|" + strconv.FormatUint(uint64(pluginHash), 10)
	fields[17] = strconv.FormatUint(uint64(canvasHash), 10)
	fields[18] = strconv.FormatUint(uint64(uaHash1), 10)
	fields[31] = strconv.FormatUint(uint64(uaHash2), 10)
	fields[33] = defaultTemplate[2] // stable init timestamp
	fields[34] = strconv.FormatUint(uint64(urlHash), 10)
	fields[36] = strconv.Itoa(docHash)

	return &fingerprint{raw: fields, hash: h[:32]}
}

// hexNibblePair reads two hex chars at byte offset off as a big-endian value.
func hexNibblePair(h string, off int) int {
	v, _ := strconv.ParseInt(h[off:off+2], 16, 32)
	return int(v)
}

func hexUint(h string) uint32 {
	v, _ := strconv.ParseUint(h, 16, 64)
	return uint32(v)
}

// hashFields are the slots re-randomised per generation, mirroring HASH_FIELDS.
// "split" rewrites only the part after `|`; "full" replaces the whole field.
var hashFields = map[int]string{
	16: "split",
	17: "full",
	18: "full",
	31: "full",
	34: "full",
	36: "full",
}

type ssxmodCookies struct {
	Itna    string
	Itna2   string
	RawData string
}

// generateCookies builds a fresh ssxmod cookie pair for the given identity.
func (f *fingerprint) generateCookies() ssxmodCookies {
	processed := make([]string, len(f.raw))
	copy(processed, f.raw)

	now := time.Now().UnixMilli()

	for idx, kind := range hashFields {
		switch kind {
		case "split":
			parts := strings.Split(processed[idx], "|")
			if len(parts) == 2 {
				processed[idx] = parts[0] + "|" + strconv.FormatUint(uint64(randomHash()), 10)
			}
		case "full":
			if idx == 36 {
				processed[idx] = strconv.Itoa(rand.Intn(91) + 10)
			} else {
				processed[idx] = strconv.FormatUint(uint64(randomHash()), 10)
			}
		}
	}

	processed[33] = strconv.FormatInt(now, 10)

	itnaData := strings.Join(processed, "^")

	// ssxmod_itna2 uses only fields 0, 1, 23, 32, 33 with a fixed 0/'' skeleton.
	itna2Parts := []string{
		processed[0], processed[1], processed[23], "0", "", "0", "", "", "0",
		"0", "0", processed[32], processed[33], "0", "0", "0", "0", "0",
	}
	itna2Data := strings.Join(itna2Parts, "^")

	return ssxmodCookies{
		Itna:    "1-" + customEncode(itnaData, true),
		Itna2:   "1-" + customEncode(itna2Data, true),
		RawData: itnaData,
	}
}
