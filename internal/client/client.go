// Package client is the HTTP client for the central portal API.
//
// It only speaks the raw-bytes and JSON endpoints described in
// API-ENDPOINTS.md. TLS verification is always on; a private CA can be added
// with the caFile argument of New.
package client

import (
	"bufio"
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
	"central-portal/internal/sanitize"
)

// ErrEmpty reports that a registry has no content yet (HTTP 404).
var ErrEmpty = errors.New("registry is empty")

const (
	// maxClipboardBytes bounds clipboard downloads. It matches the server's
	// default limit; a larger body means a misconfigured or broken server.
	maxClipboardBytes = 16 << 20
	maxErrorBytes     = 4 << 10
	maxJSONBytes      = 1 << 20
	// maxFileBytes bounds file downloads so a broken or hostile server cannot
	// fill the disk. It matches the largest file the portal is meant to store.
	maxFileBytes = 1 << 30
)

// maxRedirects bounds how many redirects the API client follows.
const maxRedirects = 10

// Client talks to one portal server.
type Client struct {
	base string
	http *http.Client
}

// New returns a client for server, for example "https://172.22.73.152:8443".
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
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}

	base := u.Scheme + "://" + u.Host
	if path := strings.TrimRight(u.EscapedPath(), "/"); path != "" {
		base += path
	}
	return &Client{
		base: base,
		http: &http.Client{Transport: transport, CheckRedirect: checkRedirect},
	}, nil
}

// checkRedirect keeps the client on the portal it was configured for. The
// portal has no reason to redirect, and following a redirect would leak
// credentials or downgrade the connection.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= maxRedirects {
		return errors.New("stopped after too many redirects")
	}
	orig := via[0].URL
	if !strings.EqualFold(req.URL.Host, orig.Host) {
		return fmt.Errorf("refusing redirect to another host: %s", sanitize.Clean(req.URL.Host))
	}
	if orig.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect from https to %s", sanitize.Clean(req.URL.Scheme))
	}
	return nil
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

// Event is one server-sent change notification from GET /api/events. Entry is
// nil when the clipboard registry is empty, which is also the snapshot sent to
// a new subscriber.
type Event struct {
	Kind  string       `json:"kind"`
	Entry *model.Entry `json:"entry"`
}

// maxEventBytes bounds one server-sent event. Entry metadata is far smaller;
// the limit only stops a broken server from growing a line without bound.
const maxEventBytes = 64 << 10

// StreamEvents reads GET /api/events and calls handle for every update until
// ctx is cancelled, the connection fails, or handle returns an error. The
// server sends the current clipboard entry as the first event, so a caller can
// reconnect and reconcile without a separate status request.
func (c *Client) StreamEvents(ctx context.Context, handle func(Event) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return networkError(err)
	}
	defer resp.Body.Close()
	if err := responseError(resp); err != nil {
		return err
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		return fmt.Errorf("event stream has content type %q, want text/event-stream", got)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 4096), maxEventBytes)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(strings.TrimSuffix(data.String(), "\n")), &ev); err != nil {
				return fmt.Errorf("decode event: %w", err)
			}
			data.Reset()
			if err := handle(ev); err != nil {
				return err
			}
			continue
		}
		// A colon starts a comment, which the server uses as a heartbeat.
		if strings.HasPrefix(line, ":") {
			continue
		}
		value, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data.WriteString(strings.TrimPrefix(value, " "))
		data.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("event stream closed")
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
	if contentType == "" {
		contentType = model.ContentTypeText
	}
	var body io.Reader = bytes.NewReader(data)
	if w != nil {
		body = io.TeeReader(body, w)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/api/clipboard", body)
	if err != nil {
		return model.Entry{}, err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", contentType)

	resp, err := c.http.Do(req)
	if err != nil {
		return model.Entry{}, networkError(err)
	}
	defer resp.Body.Close()
	return decodeEntry(resp)
}

// PutFile replaces the file registry with the bytes read from r. size must be
// the exact number of bytes in r. The bytes are copied to w as they are sent
// (w may be nil).
func (c *Client) PutFile(ctx context.Context, name string, r io.Reader, size int64, w io.Writer) (model.Entry, error) {
	var body io.Reader = r
	if w != nil {
		body = io.TeeReader(r, w)
	}
	target := c.base + "/api/file?name=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, body)
	if err != nil {
		return model.Entry{}, err
	}
	req.ContentLength = size

	resp, err := c.http.Do(req)
	if err != nil {
		return model.Entry{}, networkError(err)
	}
	defer resp.Body.Close()
	return decodeEntry(resp)
}

// GetFile downloads the file registry into dst and returns the byte count.
// A response that declares or delivers more than maxFileBytes is rejected.
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
	if resp.ContentLength > maxFileBytes {
		return 0, fmt.Errorf("file response exceeds %d MiB", maxFileBytes>>20)
	}
	n, err := io.Copy(dst, io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return n, err
	}
	if n > maxFileBytes {
		return 0, fmt.Errorf("file response exceeds %d MiB", maxFileBytes>>20)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return n, fmt.Errorf("file response truncated: got %d of %d bytes", n, resp.ContentLength)
	}
	return n, nil
}

// decodeEntry reads the entry JSON of a successful upload response.
func decodeEntry(resp *http.Response) (model.Entry, error) {
	var e model.Entry
	if err := responseError(resp); err != nil {
		return e, err
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(&e); err != nil {
		return e, fmt.Errorf("decode response: %w", err)
	}
	return e, nil
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
