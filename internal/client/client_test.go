package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"central-portal/internal/model"
)

// testClient starts a TLS server, writes its certificate to a temporary file
// and returns a client configured to trust that CA.
func testClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.crt")
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemData, 0o644); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	c, err := New(srv.URL, ca)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func TestStatus(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"clipboard":{"size":42,"sha256":"ab","contentType":"image/png","updatedAt":"2026-11-18T09:12:33Z"},"file":null}`)
	})
	c, _ := testClient(t, h)

	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Clipboard == nil || st.Clipboard.Size != 42 || st.Clipboard.SHA256 != "ab" {
		t.Fatalf("unexpected clipboard entry: %+v", st.Clipboard)
	}
	if st.Clipboard.ContentType != "image/png" {
		t.Fatalf("contentType = %q, want image/png", st.Clipboard.ContentType)
	}
	if st.File != nil {
		t.Fatalf("unexpected file entry: %+v", st.File)
	}
	if got := st.Clipboard.UpdatedAt.UTC().Format(time.RFC3339); got != "2026-11-18T09:12:33Z" {
		t.Fatalf("updatedAt = %q", got)
	}
}

func TestClipboardRoundtrip(t *testing.T) {
	var stored []byte
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				http.Error(w, "wrong content type: "+ct, http.StatusBadRequest)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			stored = body
			json.NewEncoder(w).Encode(model.Entry{Size: int64(len(body)), UpdatedAt: time.Now().UTC()})
		case http.MethodGet:
			if stored == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", model.ContentTypeText)
			w.Header().Set("Content-Length", strconv.Itoa(len(stored)))
			w.Write(stored)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	c, _ := testClient(t, h)
	ctx := context.Background()

	const text = "hello, portal"
	var upload bytes.Buffer
	// An empty type means text; the client must send the canonical one.
	entry, err := c.PutClipboard(ctx, []byte(text), "", &upload)
	if err != nil {
		t.Fatalf("PutClipboard: %v", err)
	}
	if entry.Size != int64(len(text)) {
		t.Fatalf("entry.Size = %d, want %d", entry.Size, len(text))
	}
	if upload.String() != text {
		t.Fatalf("upload progress got %q", upload.String())
	}

	var download bytes.Buffer
	data, contentType, err := c.GetClipboard(ctx, &download)
	if err != nil {
		t.Fatalf("GetClipboard: %v", err)
	}
	if string(data) != text {
		t.Fatalf("GetClipboard = %q, want %q", data, text)
	}
	if contentType != model.ContentTypeText {
		t.Fatalf("contentType = %q, want %q", contentType, model.ContentTypeText)
	}
	if download.String() != text {
		t.Fatalf("download progress got %q", download.String())
	}
}

func TestImageClipboardRoundtrip(t *testing.T) {
	payload := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	var gotType string
	var stored []byte
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			gotType = r.Header.Get("Content-Type")
			stored, _ = io.ReadAll(r.Body)
			json.NewEncoder(w).Encode(model.Entry{
				Size:        int64(len(stored)),
				ContentType: "image/png",
				UpdatedAt:   time.Now().UTC(),
			})
		case http.MethodGet:
			if stored == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			w.Write(stored)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	c, _ := testClient(t, h)
	ctx := context.Background()

	entry, err := c.PutClipboard(ctx, payload, "image/png", nil)
	if err != nil {
		t.Fatalf("PutClipboard: %v", err)
	}
	if gotType != "image/png" {
		t.Fatalf("server saw Content-Type %q, want image/png", gotType)
	}
	if entry.ContentType != "image/png" {
		t.Fatalf("entry.ContentType = %q, want image/png", entry.ContentType)
	}

	data, contentType, err := c.GetClipboard(ctx, nil)
	if err != nil {
		t.Fatalf("GetClipboard: %v", err)
	}
	if contentType != "image/png" {
		t.Fatalf("downloaded contentType = %q, want image/png", contentType)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("downloaded bytes = %v, want %v", data, payload)
	}
}

func TestPutFile(t *testing.T) {
	const name = "re port ü.bin"
	payload := []byte{0x00, 0x01, 0xff}
	var gotName string
	var gotBody []byte
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/file" {
			http.NotFound(w, r)
			return
		}
		gotName = r.URL.Query().Get("name")
		gotBody, _ = io.ReadAll(r.Body)
		json.NewEncoder(w).Encode(model.Entry{Name: gotName, Size: int64(len(gotBody)), UpdatedAt: time.Now().UTC()})
	})
	c, _ := testClient(t, h)

	var seen bytes.Buffer
	entry, err := c.PutFile(context.Background(), name, bytes.NewReader(payload), int64(len(payload)), &seen)
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if gotName != name {
		t.Fatalf("server saw name %q, want %q", gotName, name)
	}
	if !bytes.Equal(gotBody, payload) {
		t.Fatalf("server saw body %v, want %v", gotBody, payload)
	}
	if entry.Name != name || entry.Size != int64(len(payload)) {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if !bytes.Equal(seen.Bytes(), payload) {
		t.Fatalf("progress saw %v, want %v", seen.Bytes(), payload)
	}
}

func TestGetFile(t *testing.T) {
	payload := bytes.Repeat([]byte("abc"), 1000)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/file" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Write(payload)
	})
	c, _ := testClient(t, h)

	var dst bytes.Buffer
	n, err := c.GetFile(context.Background(), &dst)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("GetFile returned %d bytes, want %d", n, len(payload))
	}
	if !bytes.Equal(dst.Bytes(), payload) {
		t.Fatal("downloaded bytes differ")
	}
}

func TestEmptyRegistry(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	c, _ := testClient(t, h)
	ctx := context.Background()

	if _, _, err := c.GetClipboard(ctx, nil); !errors.Is(err, ErrEmpty) {
		t.Fatalf("GetClipboard error = %v, want ErrEmpty", err)
	}
	if _, err := c.GetFile(ctx, io.Discard); !errors.Is(err, ErrEmpty) {
		t.Fatalf("GetFile error = %v, want ErrEmpty", err)
	}
}

func TestServerErrorText(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/clipboard":
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			io.WriteString(w, "upload exceeds the 16 MiB limit\n")
		case "/api/status":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})
	c, _ := testClient(t, h)
	ctx := context.Background()

	_, err := c.PutClipboard(ctx, []byte("x"), model.ContentTypeText, nil)
	if err == nil || err.Error() != "server: upload exceeds the 16 MiB limit" {
		t.Fatalf("PutClipboard error = %v", err)
	}

	_, err = c.Status(ctx)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Status error = %v, want the status line", err)
	}
}

func TestUnsupportedMediaType(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		io.WriteString(w, "unsupported clipboard media type\n")
	})
	c, _ := testClient(t, h)

	_, err := c.PutClipboard(context.Background(), []byte("<svg/>"), "image/svg+xml", nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported clipboard media type") {
		t.Fatalf("PutClipboard error = %v, want the server message", err)
	}
}

func TestBadURL(t *testing.T) {
	for _, server := range []string{"", "example.com", "ftp://example.com", "https://", "://bad"} {
		if _, err := New(server, ""); err == nil {
			t.Errorf("New(%q) succeeded, want an error", server)
		}
	}
}

func TestCAFileWithoutCertificate(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(ca, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New("https://example.com", ca); err == nil {
		t.Fatal("New accepted a CA file without certificates")
	}
}

func TestTLSVerificationIsRequired(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"clipboard":null,"file":null}`)
	})
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)

	untrusted, err := New(srv.URL, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := untrusted.Status(context.Background()); err == nil {
		t.Fatal("request without the CA succeeded; TLS verification may be disabled")
	} else {
		var certErr *tls.CertificateVerificationError
		if !errors.As(err, &certErr) {
			t.Fatalf("error is not a certificate verification error: %v", err)
		}
		if !strings.Contains(err.Error(), "cpctl ca") {
			t.Fatalf("error lacks the CA hint: %v", err)
		}
	}

	ca := filepath.Join(t.TempDir(), "ca.crt")
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemData, 0o644); err != nil {
		t.Fatal(err)
	}
	trusted, err := New(srv.URL, ca)
	if err != nil {
		t.Fatalf("New with CA: %v", err)
	}
	if _, err := trusted.Status(context.Background()); err != nil {
		t.Fatalf("request with CA failed: %v", err)
	}
}

// TestStatusResponseHeaderTimeout covers a server that completes the TLS
// handshake but never sends response headers: Status must fail instead of
// hanging forever.
func TestStatusResponseHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	})
	c, _ := testClient(t, h)
	t.Cleanup(func() { close(release) })

	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.http.Transport)
	}
	tr.ResponseHeaderTimeout = 50 * time.Millisecond

	errc := make(chan error, 1)
	go func() {
		_, err := c.Status(context.Background())
		errc <- err
	}()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Status succeeded, want a timeout error")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "timeout") {
			t.Fatalf("error = %v, want a timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Status hung waiting for response headers")
	}
}

// TestGetFileRejectsOversizedResponse checks the size cap is enforced from
// Content-Length alone, before any body is read or written.
func TestGetFileRejectsOversizedResponse(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(maxFileBytes+1, 10))
		w.WriteHeader(http.StatusOK)
	})
	c, _ := testClient(t, h)

	var dst bytes.Buffer
	n, err := c.GetFile(context.Background(), &dst)
	if err == nil {
		t.Fatal("GetFile succeeded, want an oversize error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want a size-limit message", err)
	}
	if n != 0 {
		t.Fatalf("GetFile returned %d bytes, want 0", n)
	}
	if dst.Len() != 0 {
		t.Fatalf("GetFile wrote %d bytes to dst, want 0", dst.Len())
	}
}

// TestRedirectRejected checks that the client does not follow a redirect away
// from the configured portal.
func TestRedirectRejected(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.example/steal", http.StatusFound)
	})
	c, _ := testClient(t, h)

	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("Status followed a redirect, want an error")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("error = %v, want a redirect error", err)
	}
}
