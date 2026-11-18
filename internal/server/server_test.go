package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"central-portal/internal/model"
	"central-portal/internal/store"
)

const testCAPEM = `-----BEGIN CERTIFICATE-----
MIIBdGVzdA==
-----END CERTIFICATE-----
`

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, []byte(testCAPEM), 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	web := fstest.MapFS{
		"index.html":           &fstest.MapFile{Data: []byte("<html>portal</html>")},
		"app.js":               &fstest.MapFile{Data: []byte("console.log(1)")},
		"manifest.webmanifest": &fstest.MapFile{Data: []byte(`{"name":"Portal"}`)},
		"icons/icon-192.png":   &fstest.MapFile{Data: []byte("png")},
	}
	return New(Config{
		Store:        st,
		Web:          web,
		CACertPath:   caPath,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxClipboard: 16,
		MaxFile:      16,
	})
}

func do(t *testing.T, h http.Handler, method, target, body string, chunked bool) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if chunked {
		req.ContentLength = -1
	}
	req.RemoteAddr = "192.0.2.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeEntry(t *testing.T, rec *httptest.ResponseRecorder) model.Entry {
	t.Helper()
	var entry model.Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &entry); err != nil {
		t.Fatalf("decode entry %q: %v", rec.Body.String(), err)
	}
	return entry
}

func getStatus(t *testing.T, h http.Handler) model.Status {
	t.Helper()
	rec := do(t, h, "GET", "/api/status", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d, want 200", rec.Code)
	}
	var status model.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return status
}

func TestStatusEmpty(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, "GET", "/api/status", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"clipboard":null`) || !strings.Contains(body, `"file":null`) {
		t.Errorf("empty status = %s, want null registries", body)
	}
	status := getStatus(t, h)
	if status.Clipboard != nil || status.File != nil {
		t.Errorf("empty status = %+v, want nil entries", status)
	}
}

func TestClipboardRoundTrip(t *testing.T) {
	h := newTestHandler(t)

	rec := do(t, h, "GET", "/api/clipboard", "", false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty GET /api/clipboard = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "clipboard registry is empty") {
		t.Errorf("body = %q", rec.Body.String())
	}

	rec = do(t, h, "PUT", "/api/clipboard", "hello", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/clipboard = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	entry := decodeEntry(t, rec)
	if entry.Size != 5 || entry.Name != "" || len(entry.SHA256) != 64 || entry.UpdatedAt.IsZero() {
		t.Errorf("entry = %+v", entry)
	}
	if entry.ContentType != model.ContentTypeText {
		t.Errorf("ContentType = %q, want %q", entry.ContentType, model.ContentTypeText)
	}

	rec = do(t, h, "GET", "/api/clipboard", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/clipboard = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "hello" {
		t.Errorf("body = %q, want hello", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "5" {
		t.Errorf("Content-Length = %q, want 5", got)
	}
	if rec.Header().Get("Last-Modified") == "" {
		t.Error("Last-Modified missing")
	}

	status := getStatus(t, h)
	if status.Clipboard == nil || status.Clipboard.Size != 5 {
		t.Errorf("status clipboard = %+v", status.Clipboard)
	}
}

func TestClipboardEmptyUpload(t *testing.T) {
	h := newTestHandler(t)
	for _, chunked := range []bool{false, true} {
		rec := do(t, h, "PUT", "/api/clipboard", "", chunked)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("empty PUT (chunked=%v) = %d, want 400", chunked, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "empty clipboard upload") {
			t.Errorf("body = %q", rec.Body.String())
		}
	}
	status := getStatus(t, h)
	if status.Clipboard != nil {
		t.Errorf("empty upload stored an entry: %+v", status.Clipboard)
	}
}

func TestClipboardImage(t *testing.T) {
	h := newTestHandler(t)
	png := "\x89PNG\r\n\x1a\nfake"
	req := httptest.NewRequest("PUT", "/api/clipboard", strings.NewReader(png))
	req.Header.Set("Content-Type", "image/png")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT image = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if entry := decodeEntry(t, rec); entry.ContentType != "image/png" {
		t.Errorf("ContentType = %q, want image/png", entry.ContentType)
	}
	rec = do(t, h, "GET", "/api/clipboard", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET image = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	if rec.Body.String() != png {
		t.Errorf("body = %q, want image bytes", rec.Body.String())
	}
	if status := getStatus(t, h); status.Clipboard == nil || status.Clipboard.ContentType != "image/png" {
		t.Errorf("status clipboard = %+v", status.Clipboard)
	}
}

func TestClipboardTextContentTypeParameters(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest("PUT", "/api/clipboard", strings.NewReader("text"))
	req.Header.Set("Content-Type", "text/plain; charset=iso-8859-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", rec.Code)
	}
	if entry := decodeEntry(t, rec); entry.ContentType != model.ContentTypeText {
		t.Errorf("ContentType = %q, want %q", entry.ContentType, model.ContentTypeText)
	}
}

func TestClipboardUnsupportedMediaType(t *testing.T) {
	h := newTestHandler(t)
	if rec := do(t, h, "PUT", "/api/clipboard", "keep", false); rec.Code != http.StatusOK {
		t.Fatalf("initial PUT = %d", rec.Code)
	}
	for _, contentType := range []string{"image/svg+xml", "text/html", "application/octet-stream", "not a media type"} {
		req := httptest.NewRequest("PUT", "/api/clipboard", strings.NewReader("<svg/>"))
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("PUT %q = %d, want 415", contentType, rec.Code)
		}
	}
	rec := do(t, h, "GET", "/api/clipboard", "", false)
	if rec.Body.String() != "keep" {
		t.Errorf("clipboard after rejected upload = %q, want keep", rec.Body.String())
	}
}

func TestClipboardTooLargeKeepsOldContent(t *testing.T) {
	h := newTestHandler(t)
	if rec := do(t, h, "PUT", "/api/clipboard", "ok", false); rec.Code != http.StatusOK {
		t.Fatalf("initial PUT = %d", rec.Code)
	}
	for _, chunked := range []bool{false, true} {
		rec := do(t, h, "PUT", "/api/clipboard", strings.Repeat("a", 17), chunked)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("oversize PUT (chunked=%v) = %d, want 413", chunked, rec.Code)
		}
	}
	rec := do(t, h, "GET", "/api/clipboard", "", false)
	if rec.Body.String() != "ok" {
		t.Errorf("clipboard after oversize upload = %q, want ok", rec.Body.String())
	}
}

func TestFileRoundTripUnicodeName(t *testing.T) {
	h := newTestHandler(t)
	name := "héllo 世界.txt"
	body := "file contents"

	rec := do(t, h, "PUT", "/api/file?name="+url.QueryEscape(name), body, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/file = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	entry := decodeEntry(t, rec)
	if entry.Name != name || entry.Size != int64(len(body)) {
		t.Errorf("entry = %+v", entry)
	}

	rec = do(t, h, "GET", "/api/file", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/file = %d, want 200", rec.Code)
	}
	if rec.Body.String() != body {
		t.Errorf("body = %q, want %q", rec.Body.String(), body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, `filename="download"`) || !strings.Contains(disposition, "filename*=UTF-8''") {
		t.Errorf("Content-Disposition = %q", disposition)
	}

	status := getStatus(t, h)
	if status.File == nil || status.File.Name != name {
		t.Errorf("status file = %+v", status.File)
	}
}

func TestFileASCIIAndRange(t *testing.T) {
	h := newTestHandler(t)
	if rec := do(t, h, "PUT", "/api/file?name=notes.txt", "0123456789", false); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d", rec.Code)
	}
	rec := do(t, h, "GET", "/api/file", "", false)
	if got := rec.Header().Get("Content-Disposition"); got != "attachment; filename=notes.txt" {
		t.Errorf("Content-Disposition = %q", got)
	}

	req := httptest.NewRequest("GET", "/api/file", nil)
	req.Header.Set("Range", "bytes=2-4")
	rangeRec := httptest.NewRecorder()
	h.ServeHTTP(rangeRec, req)
	if rangeRec.Code != http.StatusPartialContent {
		t.Fatalf("range GET = %d, want 206", rangeRec.Code)
	}
	if rangeRec.Body.String() != "234" {
		t.Errorf("range body = %q, want 234", rangeRec.Body.String())
	}
}

func TestFilePathTraversal(t *testing.T) {
	h := newTestHandler(t)
	for _, raw := range []string{"../evil.txt", `..\evil.txt`, "a/../../evil.txt"} {
		rec := do(t, h, "PUT", "/api/file?name="+url.QueryEscape(raw), "x", false)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %q = %d, want 200", raw, rec.Code)
		}
		if entry := decodeEntry(t, rec); entry.Name != "evil.txt" {
			t.Errorf("PUT %q stored name %q, want evil.txt", raw, entry.Name)
		}
	}
}

func TestFileInvalidName(t *testing.T) {
	h := newTestHandler(t)
	cases := map[string]string{
		"missing":  "/api/file",
		"empty":    "/api/file?name=",
		"dot":      "/api/file?name=.",
		"dotdot":   "/api/file?name=..",
		"slashes":  "/api/file?name=%2F",
		"control":  "/api/file?name=" + url.QueryEscape("bad\x01name"),
		"too long": "/api/file?name=" + strings.Repeat("a", maxNameRunes+1),
	}
	for label, target := range cases {
		rec := do(t, h, "PUT", target, "x", false)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d, want 400", label, rec.Code)
		}
	}
}

func TestFileZeroBytes(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, "PUT", "/api/file?name=empty.bin", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("zero-byte PUT = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if entry := decodeEntry(t, rec); entry.Size != 0 || entry.Name != "empty.bin" {
		t.Errorf("entry = %+v", entry)
	}
	rec = do(t, h, "GET", "/api/file", "", false)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("GET zero-byte file = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if status := getStatus(t, h); status.File == nil || status.File.Size != 0 {
		t.Errorf("status file = %+v", status.File)
	}
}

func TestFileTooLargeKeepsOldContent(t *testing.T) {
	h := newTestHandler(t)
	if rec := do(t, h, "PUT", "/api/file?name=small.bin", "small", false); rec.Code != http.StatusOK {
		t.Fatalf("initial PUT = %d", rec.Code)
	}
	rec := do(t, h, "PUT", "/api/file?name=big.bin", strings.Repeat("a", 17), false)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize PUT = %d, want 413", rec.Code)
	}
	rec = do(t, h, "GET", "/api/file", "", false)
	if rec.Body.String() != "small" {
		t.Errorf("file after oversize upload = %q, want small", rec.Body.String())
	}
	if status := getStatus(t, h); status.File == nil || status.File.Name != "small.bin" {
		t.Errorf("status file = %+v", status.File)
	}
}

func TestCACert(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, "GET", "/ca.crt", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ca.crt = %d, want 200", rec.Code)
	}
	if rec.Body.String() != testCAPEM {
		t.Errorf("body = %q, want test CA PEM", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/x-x509-ca-cert" {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestStatic(t *testing.T) {
	h := newTestHandler(t)
	for _, target := range []string{"/", "/index.html", "/app.js", "/icons/icon-192.png"} {
		rec := do(t, h, "GET", target, "", false)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", target, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", target, got)
		}
	}
	if rec := do(t, h, "GET", "/", "", false); !strings.Contains(rec.Body.String(), "portal") {
		t.Errorf("index body = %q", rec.Body.String())
	}
}

func TestStaticManifestContentType(t *testing.T) {
	h := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/manifest+json" {
		t.Fatalf("content type = %q", got)
	}
}

func TestUnknownPathAndDirectory(t *testing.T) {
	h := newTestHandler(t)
	for _, target := range []string{"/missing.txt", "/icons/", "/icons"} {
		rec := do(t, h, "GET", target, "", false)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, rec.Code)
		}
	}
}

func TestWrongMethod(t *testing.T) {
	h := newTestHandler(t)
	for _, tc := range []struct{ method, target string }{
		{"POST", "/api/status"},
		{"DELETE", "/api/clipboard"},
		{"POST", "/api/file"},
	} {
		rec := do(t, h, tc.method, tc.target, "", false)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.target, rec.Code)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newTestHandler(t)
	for _, target := range []string{"/api/status", "/", "/ca.crt"} {
		rec := do(t, h, "GET", target, "", false)
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s X-Content-Type-Options = %q", target, got)
		}
		if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s Referrer-Policy = %q", target, got)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != contentSecurityPolicy {
			t.Errorf("%s Content-Security-Policy = %q", target, got)
		}
	}
}
