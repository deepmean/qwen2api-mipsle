package main

// Transport layer.
//
// Uses net/http rather than a hand-rolled HTTP stack: the binary is ~2MB larger,
// but /overlay has 41MB free and the request-parsing surface is far less risky to
// get right on a device that is only reachable over SSH. RAM — the actual
// constraint on this router — is comparable either way.
//
// Content-Encoding is decoded explicitly. The browser header profile sets
// Accept-Encoding itself, which suppresses net/http's transparent
// decompression — that only happens when the transport added the header. We
// advertise only gzip and deflate, the encodings the standard library can
// actually decode.

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type httpClient struct {
	c *http.Client
}

func newHTTPClient() *httpClient {
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	// No http.Client.Timeout: it would cap the whole SSE stream. Per-call
	// deadlines come from the request context instead.
	return &httpClient{c: &http.Client{Transport: tr}}
}

// response is the subset of http.Response the upstream client needs. Body must be
// closed by the caller; closing an SSE body early tears down the connection,
// which is how the streaming handler stops generation.
type response struct {
	StatusCode int
	Header     map[string]string
	Body       io.ReadCloser
}

func (r *response) Close() error {
	if r.Body == nil {
		return nil
	}
	return r.Body.Close()
}

// cancelReadCloser cancels the request context when the body is closed, so a
// per-call deadline is released as soon as the caller is done rather than when
// the function returns.
type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// do issues one request. body is sent verbatim; pass nil for bodyless methods.
// A non-zero timeout bounds the call from dial through body completion.
func (c *httpClient) do(ctx context.Context, method, url string, headers map[string]string, body []byte, timeout time.Duration) (*response, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		return c.send(ctx, cancel, method, url, headers, rdr, body)
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	return c.send(ctx, nil, method, url, headers, rdr, body)
}

func (c *httpClient) send(ctx context.Context, cancel context.CancelFunc, method, url string, headers map[string]string, rdr io.Reader, body []byte) (*response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}

	httpResp, err := c.c.Do(req)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}

	hdr := make(map[string]string, len(httpResp.Header))
	for k, v := range httpResp.Header {
		if len(v) > 0 {
			hdr[k] = v[0]
		}
	}

	rc := httpResp.Body
	if cancel != nil {
		rc = &cancelReadCloser{ReadCloser: rc, cancel: cancel}
	}

	// The request sets Accept-Encoding explicitly (the browser profile carries
	// it), which suppresses net/http's transparent decompression — it only
	// auto-decodes when it added the header itself. So decode here.
	rc, err = decompressBody(rc, hdr)
	if err != nil {
		rc.Close()
		return nil, err
	}

	return &response{StatusCode: httpResp.StatusCode, Header: hdr, Body: rc}, nil
}

// decompressBody wraps the body when the upstream compressed it. Only gzip and
// deflate are advertised, so only those need handling; anything else is a
// protocol error rather than something to silently pass through as garbage.
func decompressBody(rc io.ReadCloser, hdr map[string]string) (io.ReadCloser, error) {
	enc := strings.ToLower(strings.TrimSpace(hdr["Content-Encoding"]))
	switch enc {
	case "", "identity":
		return rc, nil
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(rc)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		return &multiReadCloser{Reader: zr, closers: []io.Closer{zr, rc}}, nil
	case "deflate":
		// RFC 7230 specifies zlib-wrapped deflate, but some servers send the raw
		// stream. zlib.NewReader consumes the 2-byte header before it can fail,
		// so sniff the header through a buffered reader instead of trying zlib
		// first and falling back — the fallback would see a corrupted stream.
		br := bufio.NewReader(rc)
		if head, err := br.Peek(2); err == nil && isZlibHeader(head) {
			zr, err := zlib.NewReader(br)
			if err != nil {
				return nil, fmt.Errorf("deflate: %w", err)
			}
			return &multiReadCloser{Reader: zr, closers: []io.Closer{zr, rc}}, nil
		}
		fr := flate.NewReader(br)
		return &multiReadCloser{Reader: fr, closers: []io.Closer{fr, rc}}, nil
	default:
		return nil, fmt.Errorf("unsupported content-encoding %q", enc)
	}
}

// isZlibHeader reports whether the two leading bytes are a valid zlib header:
// CM=8 (deflate) in the low nibble of byte 0, and the 16-bit big-endian value
// divisible by 31, per RFC 1950.
func isZlibHeader(head []byte) bool {
	if len(head) < 2 {
		return false
	}
	if head[0]&0x0f != 8 {
		return false
	}
	return (uint16(head[0])<<8|uint16(head[1]))%31 == 0
}

// multiReadCloser closes the decoder before the underlying connection, so the
// decoder can finish reading its trailer without the socket already being gone.
type multiReadCloser struct {
	io.Reader
	closers []io.Closer
}

func (m *multiReadCloser) Close() error {
	var firstErr error
	for _, c := range m.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// readAllLimited drains a body up to limit bytes. JSON replies are bounded this
// way so a misbehaving upstream cannot grow the heap without limit.
func readAllLimited(r io.Reader, limit int) ([]byte, error) {
	var buf bytes.Buffer
	_, err := io.Copy(&buf, io.LimitReader(r, int64(limit)))
	return buf.Bytes(), err
}
