// Package server implements the HTTP API and the static frontend of the
// central portal.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"central-portal/internal/model"
	"central-portal/internal/store"
)

const (
	defaultMaxClipboard = 16 << 20
	defaultMaxFile      = 1 << 30

	// maxNameRunes limits the client-supplied file name.
	maxNameRunes = 200

	// idleTimeout is how long a single body read or response write may stall
	// before the connection is closed. It applies per operation, so large
	// transfers are fine as long as they keep making progress.
	idleTimeout = 30 * time.Second

	contentSecurityPolicy = "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'none'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; worker-src 'self'; manifest-src 'self'"
)

// ResolveListenAddr turns a configured listen address into a concrete one.
//
// The special value "auto" binds the ZeroTier IPv4 address when ztIP is set
// and otherwise falls back to loopback on port. loopback reports that fallback
// so the caller can warn. Any other value is returned unchanged, which keeps
// an explicit ":8443" or "0.0.0.0:8443" as the documented escape hatch for
// binding every interface.
func ResolveListenAddr(opt, ztIP, port string) (addr string, loopback bool) {
	if opt != "auto" {
		return opt, false
	}
	if ztIP == "" {
		return net.JoinHostPort("127.0.0.1", port), true
	}
	return net.JoinHostPort(ztIP, port), false
}

var errEmptyUpload = errors.New("empty upload")

func init() {
	// Go's built-in MIME table does not know .webmanifest, and browsers expect
	// the manifest to be served with its proper type.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// Config configures New. Zero sizes fall back to the defaults.
type Config struct {
	Store        *store.Store
	Web          fs.FS  // static frontend rooted at its own root
	CACertPath   string // path to ca.crt
	Logger       *slog.Logger
	MaxClipboard int64
	MaxFile      int64
}

// New returns the portal handler.
func New(cfg Config) http.Handler {
	h := &handler{
		store:        cfg.Store,
		web:          cfg.Web,
		caCertPath:   cfg.CACertPath,
		logger:       cfg.Logger,
		maxClipboard: cfg.MaxClipboard,
		maxFile:      cfg.MaxFile,
	}
	if h.logger == nil {
		h.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if h.maxClipboard <= 0 {
		h.maxClipboard = defaultMaxClipboard
	}
	if h.maxFile <= 0 {
		h.maxFile = defaultMaxFile
	}
	if h.web != nil {
		h.files = http.FileServerFS(h.web)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", h.getStatus)
	mux.HandleFunc("GET /api/clipboard", h.getClipboard)
	mux.HandleFunc("PUT /api/clipboard", h.putClipboard)
	mux.HandleFunc("GET /api/file", h.getFile)
	mux.HandleFunc("PUT /api/file", h.putFile)
	mux.HandleFunc("GET /ca.crt", h.getCA)
	mux.HandleFunc("GET /", h.static)
	return h.withMiddleware(mux)
}

type handler struct {
	store        *store.Store
	web          fs.FS
	files        http.Handler
	caCertPath   string
	logger       *slog.Logger
	maxClipboard int64
	maxFile      int64
}

func (h *handler) getStatus(w http.ResponseWriter, r *http.Request) {
	var status model.Status
	if e, ok := h.store.Get(model.KindClipboard); ok {
		status.Clipboard = &e
	}
	if e, ok := h.store.Get(model.KindFile); ok {
		status.File = &e
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *handler) getClipboard(w http.ResponseWriter, r *http.Request) {
	f, e, err := h.store.Open(model.KindClipboard)
	if errors.Is(err, store.ErrEmpty) {
		http.Error(w, "clipboard registry is empty", http.StatusNotFound)
		return
	}
	if err != nil {
		h.logger.Error("open clipboard", "error", err)
		http.Error(w, "cannot read clipboard", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	contentType := e.ContentType
	if contentType == "" {
		contentType = model.ContentTypeText
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", e.UpdatedAt, f)
}

func (h *handler) putClipboard(w http.ResponseWriter, r *http.Request) {
	contentType, ok := clipboardContentType(r.Header.Get("Content-Type"))
	if !ok {
		http.Error(w, "unsupported clipboard media type", http.StatusUnsupportedMediaType)
		return
	}
	if r.ContentLength == 0 {
		http.Error(w, "empty clipboard upload", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxClipboard)
	if r.ContentLength > h.maxClipboard {
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
		return
	}
	e, err := h.store.Put(model.KindClipboard, "", contentType, &nonEmptyReader{r: r.Body}, h.maxClipboard)
	if err != nil {
		h.uploadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (h *handler) getFile(w http.ResponseWriter, r *http.Request) {
	f, e, err := h.store.Open(model.KindFile)
	if errors.Is(err, store.ErrEmpty) {
		http.Error(w, "file registry is empty", http.StatusNotFound)
		return
	}
	if err != nil {
		h.logger.Error("open file", "error", err)
		http.Error(w, "cannot read file", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition(e.Name))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", e.UpdatedAt, f)
}

func (h *handler) putFile(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("name")
	if raw == "" {
		http.Error(w, "missing file name", http.StatusBadRequest)
		return
	}
	name, ok := sanitizeName(raw)
	if !ok {
		http.Error(w, "invalid file name", http.StatusBadRequest)
		return
	}
	if r.ContentLength > h.maxFile {
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxFile)
	e, err := h.store.Put(model.KindFile, name, "", r.Body, h.maxFile)
	if err != nil {
		h.uploadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (h *handler) getCA(w http.ResponseWriter, r *http.Request) {
	if h.caCertPath == "" {
		http.Error(w, "CA certificate not found", http.StatusNotFound)
		return
	}
	data, err := os.ReadFile(h.caCertPath)
	if errors.Is(err, os.ErrNotExist) {
		http.Error(w, "CA certificate not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.logger.Error("read CA certificate", "error", err)
		http.Error(w, "cannot read CA certificate", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

// static serves the embedded frontend. Directories are never listed and only
// existing regular files are returned.
func (h *handler) static(w http.ResponseWriter, r *http.Request) {
	if h.files == nil {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	f, err := h.web.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	info, err := f.Stat()
	f.Close()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	if name == "index.html" {
		// FileServer would redirect /index.html to ./, so serve it as /.
		clone := r.Clone(r.Context())
		clone.URL.Path = "/"
		h.files.ServeHTTP(w, clone)
		return
	}
	h.files.ServeHTTP(w, r)
}

// uploadError maps storage errors to client responses without leaking details.
func (h *handler) uploadError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxErr), errors.Is(err, store.ErrTooLarge):
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
	case errors.Is(err, errEmptyUpload):
		http.Error(w, "empty clipboard upload", http.StatusBadRequest)
	default:
		h.logger.Error("upload failed", "error", err)
		http.Error(w, "upload failed", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// clipboardContentType maps the request media type to a stored type. It only
// accepts text and raster images so registry content can never execute in the
// portal origin.
func clipboardContentType(header string) (string, bool) {
	if header == "" {
		return model.ContentTypeText, true
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return "", false
	}
	switch mediaType {
	case "text/plain":
		return model.ContentTypeText, true
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return mediaType, true
	default:
		return "", false
	}
}

// sanitizeName reduces a client-supplied name to a safe basename.
func sanitizeName(raw string) (string, bool) {
	name := strings.ReplaceAll(raw, `\`, "/")
	name = path.Base(name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.Trim(name, "/") == "" {
		return "", false
	}
	for _, r := range name {
		if unsafeNameRune(r) {
			return "", false
		}
	}
	if utf8.RuneCountInString(name) > maxNameRunes {
		return "", false
	}
	return name, true
}

// unsafeNameRune reports characters that can spoof or reorder a displayed name:
// controls, bidi overrides/isolates, zero-width marks, a BOM, and line or
// paragraph separators. Ordinary Unicode, including emoji and their joiners, is
// allowed.
func unsafeNameRune(r rune) bool {
	if unicode.IsControl(r) {
		return true
	}
	switch {
	case r >= 0x202a && r <= 0x202e, // LRE, RLE, PDF, LRO, RLO
		r >= 0x2066 && r <= 0x2069, // LRI, RLI, FSI, PDI
		r == 0x200b,                // zero-width space
		r == 0x200c,                // zero-width non-joiner
		r == 0x200e,                // left-to-right mark
		r == 0x200f,                // right-to-left mark
		r == 0xfeff,                // zero-width no-break space / BOM
		r == 0x2028,                // line separator
		r == 0x2029:                // paragraph separator
		return true
	}
	return false
}

// contentDisposition builds a safe attachment header for name. Non-ASCII
// names use the RFC 5987 form so clients keep the original name.
func contentDisposition(name string) string {
	ascii := true
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		if value := mime.FormatMediaType("attachment", map[string]string{"filename": name}); value != "" {
			return value
		}
	}
	encoded := strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
	return `attachment; filename="download"; filename*=UTF-8''` + encoded
}

// nonEmptyReader turns an immediately empty body into errEmptyUpload so the
// store leaves the previous clipboard untouched.
type nonEmptyReader struct {
	r    io.Reader
	read bool
}

func (n *nonEmptyReader) Read(p []byte) (int, error) {
	k, err := n.r.Read(p)
	if k > 0 {
		n.read = true
		return k, err
	}
	if !n.read && err == io.EOF {
		return 0, errEmptyUpload
	}
	return 0, err
}

// withMiddleware adds security headers, default caching, per-operation I/O
// deadlines, request logging and panic recovery.
func (h *handler) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rc := http.NewResponseController(w)
		sw := &statusWriter{ResponseWriter: &deadlineWriter{ResponseWriter: w, rc: rc}, status: http.StatusOK}
		sw.Header().Set("Cache-Control", "no-store")
		sw.Header().Set("X-Content-Type-Options", "nosniff")
		sw.Header().Set("Referrer-Policy", "no-referrer")
		sw.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &deadlineReader{r: r.Body, rc: rc}
		}
		defer func() {
			if v := recover(); v != nil {
				h.logger.Error("panic serving request", "method", r.Method, "path", r.URL.Path, "panic", v)
				if !sw.wrote {
					http.Error(sw, "internal server error", http.StatusInternalServerError)
				}
			}
			h.logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"bytes", sw.bytes,
				"duration", time.Since(start),
				"remote", r.RemoteAddr,
			)
		}()
		next.ServeHTTP(sw, r)
	})
}

// statusWriter records the response status and size for logging.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.status = code
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// Unwrap gives http.ResponseController access to the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// deadlineReader refreshes the read deadline before every body read so a slow
// or stalled upload cannot hold a connection forever. There is deliberately no
// total timeout: a large upload may run as long as it keeps progressing.
type deadlineReader struct {
	r  io.ReadCloser
	rc *http.ResponseController
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	_ = d.rc.SetReadDeadline(time.Now().Add(idleTimeout))
	return d.r.Read(p)
}

func (d *deadlineReader) Close() error {
	return d.r.Close()
}

// deadlineWriter refreshes the write deadline before every response write, so a
// client that stops reading cannot pin a download forever.
type deadlineWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func (w *deadlineWriter) Write(p []byte) (int, error) {
	_ = w.rc.SetWriteDeadline(time.Now().Add(idleTimeout))
	return w.ResponseWriter.Write(p)
}

// Unwrap keeps http.ResponseController (and Flush/Hijack helpers) working.
func (w *deadlineWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
