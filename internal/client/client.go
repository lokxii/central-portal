// Package client is the HTTP client for the central portal API.
//
// It only speaks the raw-bytes and JSON endpoints described in
// API-ENDPOINTS.md. TLS verification is always on; a private CA can be added
// with the caFile argument of New.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"central-portal/internal/model"
)

// ErrEmpty reports that a registry has no content yet (HTTP 404).
var ErrEmpty = errors.New("registry is empty")

const (
	// maxClipboardBytes bounds clipboard downloads. It matches the server's
	// default limit; a larger body means a misconfigured or broken server.
	maxClipboardBytes = 16 << 20
	maxErrorBytes     = 4 << 10
	maxJSONBytes      = 1 << 20
)

// Client talks to one portal server.
type Client struct {
	base string
	http *http.Client
}

// New returns a client for server, for example "https://portal.example:8443".
//
// When caFile is non-empty its certificates are added to the system roots so
// the portal's self-signed CA is trusted. Certificate verification is always
// enabled and cannot be disabled.
func New(server, caFile string) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(server))
	if err != nil {
		return nil, fmt.Errorf("invalid server URL %q: %w", server, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid server URL %q: scheme must be http or https", server)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid server URL %q: missing host", server)
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pemData, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pemData) {
			return nil, fmt.Errorf("CA file %s contains no certificates", caFile)
		}
		tlsConfig.RootCAs = pool
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}

	base := u.Scheme + "://" + u.Host
	if path := strings.TrimRight(u.EscapedPath(), "/"); path != "" {
		base += path
	}
	return &Client{base: base, http: &http.Client{Transport: transport}}, nil
}

// Status returns the metadata of both registries.
func (c *Client) Status(ctx context.Context) (model.Status, error) {
	var st model.Status
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/status", nil)
	if err != nil {
		return st, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return st, networkError(err)
	}
	defer resp.Body.Close()
	if err := responseError(resp); err != nil {
		return st, err
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(&st); err != nil {
		return st, fmt.Errorf("decode status: %w", err)
	}
	return st, nil
}

// GetClipboard downloads the clipboard registry. The raw body is copied to w
// as it arrives (w may be nil) and returned together with the media type of
// the stored entry.
func (c *Client) GetClipboard(ctx context.Context, w io.Writer) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/clipboard", nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", networkError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", ErrEmpty
	}
	if err := responseError(resp); err != nil {
		return nil, "", err
	}

	var r io.Reader = resp.Body
	if w != nil {
		r = io.TeeReader(resp.Body, w)
	}
	data, err := io.ReadAll(io.LimitReader(r, maxClipboardBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxClipboardBytes {
		return nil, "", fmt.Errorf("clipboard response exceeds %d MiB", maxClipboardBytes>>20)
	}
	if resp.ContentLength >= 0 && int64(len(data)) != resp.ContentLength {
		return nil, "", fmt.Errorf("clipboard response truncated: got %d of %d bytes", len(data), resp.ContentLength)
	}
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = model.ContentTypeText
	}
	return data, contentType, nil
}

// PutClipboard replaces the clipboard registry with data. contentType selects
// the stored entry type; an empty value means text. The uploaded bytes are
// copied to w as they are sent (w may be nil).
func (c *Client) PutClipboard(ctx context.Context, data []byte, contentType string, w io.Writer) (model.Entry, error) {
	var e model.Entry
	if contentType == "" {
		contentType = model.ContentTypeText
	}
	var body io.Reader = bytes.NewReader(data)
	if w != nil {
		body = io.TeeReader(body, w)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/api/clipboard", body)
	if err != nil {
		return e, err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", contentType)

	resp, err := c.http.Do(req)
	if err != nil {
		return e, networkError(err)
	}
	defer resp.Body.Close()
	if err := responseError(resp); err != nil {
		return e, err
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(&e); err != nil {
		return e, fmt.Errorf("decode response: %w", err)
	}
	return e, nil
}

// PutFile replaces the file registry with the bytes read from r. size must be
// the exact number of bytes in r. The bytes are copied to w as they are sent
// (w may be nil).
func (c *Client) PutFile(ctx context.Context, name string, r io.Reader, size int64, w io.Writer) (model.Entry, error) {
	var e model.Entry
	var body io.Reader = r
	if w != nil {
		body = io.TeeReader(r, w)
	}
	target := c.base + "/api/file?name=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, body)
	if err != nil {
		return e, err
	}
	req.ContentLength = size

	resp, err := c.http.Do(req)
	if err != nil {
		return e, networkError(err)
	}
	defer resp.Body.Close()
	if err := responseError(resp); err != nil {
		return e, err
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(&e); err != nil {
		return e, fmt.Errorf("decode response: %w", err)
	}
	return e, nil
}

// GetFile downloads the file registry into dst and returns the byte count.
func (c *Client) GetFile(ctx context.Context, dst io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/file", nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, networkError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, ErrEmpty
	}
	if err := responseError(resp); err != nil {
		return 0, err
	}
	n, err := io.Copy(dst, resp.Body)
	if err != nil {
		return n, err
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return n, fmt.Errorf("file response truncated: got %d of %d bytes", n, resp.ContentLength)
	}
	return n, nil
}

// responseError converts a non-2xx response into an error that carries the
// server's message, or the status line when the body is empty.
func responseError(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("server: %s", msg)
}

// networkError adds a hint to certificate failures. The original error is
// preserved so callers can still inspect it.
func networkError(err error) error {
	var certErr *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &certErr) || errors.As(err, &unknown) {
		return fmt.Errorf("%w (hint: run \"cpctl ca\" to install the portal CA)", err)
	}
	return err
}
