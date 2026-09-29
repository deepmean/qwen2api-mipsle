package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"strings"
	"testing"
)

type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }

func TestDecompressBodyGzip(t *testing.T) {
	payload := []byte(`{"data":[{"id":"qwen3.6-plus"}]}`)

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(payload)
	zw.Close()

	// The compressed body must start with the gzip magic, which is what the
	// upstream actually returned before this was handled.
	if buf.Bytes()[0] != 0x1f {
		t.Fatalf("fixture is not gzip: %x", buf.Bytes()[:2])
	}

	rc, err := decompressBody(nopCloser{bytes.NewReader(buf.Bytes())}, map[string]string{"Content-Encoding": "gzip"})
	if err != nil {
		t.Fatalf("decompressBody: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

func TestDecompressBodyIdentity(t *testing.T) {
	payload := []byte("plain")
	rc, err := decompressBody(nopCloser{bytes.NewReader(payload)}, map[string]string{})
	if err != nil {
		t.Fatalf("decompressBody: %v", err)
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

func TestDecompressBodyDeflateZlibAndRaw(t *testing.T) {
	payload := []byte("deflate payload")

	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	zw.Write(payload)
	zw.Close()

	rc, err := decompressBody(nopCloser{bytes.NewReader(zbuf.Bytes())}, map[string]string{"Content-Encoding": "deflate"})
	if err != nil {
		t.Fatalf("zlib: %v", err)
	}
	if got, _ := io.ReadAll(rc); !bytes.Equal(got, payload) {
		t.Fatalf("zlib got %q", got)
	}

	// Raw (headerless) deflate, which some servers send despite RFC 7230.
	var fbuf bytes.Buffer
	fw, _ := flate.NewWriter(&fbuf, flate.DefaultCompression)
	fw.Write(payload)
	fw.Close()

	rc, err = decompressBody(nopCloser{bytes.NewReader(fbuf.Bytes())}, map[string]string{"Content-Encoding": "deflate"})
	if err != nil {
		t.Fatalf("raw deflate: %v", err)
	}
	if got, _ := io.ReadAll(rc); !bytes.Equal(got, payload) {
		t.Fatalf("raw deflate got %q", got)
	}
}

func TestDecompressBodyRejectsUnknownEncoding(t *testing.T) {
	// br/zstd are not advertised and must fail loudly rather than be handed to
	// the JSON parser as garbage.
	_, err := decompressBody(nopCloser{strings.NewReader("x")}, map[string]string{"Content-Encoding": "br"})
	if err == nil {
		t.Fatal("expected an error for brotli")
	}
}
