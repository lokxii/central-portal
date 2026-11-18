package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"central-portal/internal/model"
)

func TestPutGetOpen(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Put(model.KindClipboard, "", model.ContentTypeText, strings.NewReader("hello"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Size != 5 || entry.Name != "" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	sum := sha256.Sum256([]byte("hello"))
	if entry.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("unexpected sha256: %s", entry.SHA256)
	}

	got, ok := s.Get(model.KindClipboard)
	if !ok {
		t.Fatal("registry reported empty")
	}
	if got.Size != entry.Size || got.SHA256 != entry.SHA256 || !got.UpdatedAt.Equal(entry.UpdatedAt) {
		t.Fatalf("metadata mismatch: got %+v want %+v", got, entry)
	}

	f, _, err := s.Open(model.KindClipboard)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("content = %q", data)
	}
}

func TestOpenEmpty(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(model.KindClipboard); ok {
		t.Fatal("empty store returned metadata")
	}
	if _, _, err := s.Open(model.KindClipboard); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
}

func TestOverwrite(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(model.KindFile, "a.txt", "", strings.NewReader("one"), 1024); err != nil {
		t.Fatal(err)
	}
	entry, err := s.Put(model.KindFile, "b.txt", "", strings.NewReader("second"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "b.txt" || entry.Size != 6 {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	f, meta, err := s.Open(model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("content = %q", data)
	}
	if meta.Name != "b.txt" {
		t.Fatalf("name = %q", meta.Name)
	}
}

func TestTooLargeKeepsPrevious(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(model.KindClipboard, "", model.ContentTypeText, strings.NewReader("small"), 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(model.KindClipboard, "", model.ContentTypeText, strings.NewReader("too large"), 5); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	f, _, err := s.Open(model.KindClipboard)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	if string(data) != "small" {
		t.Fatalf("previous content lost: %q", data)
	}
}

func TestLimitBoundary(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(model.KindFile, "exact", "", strings.NewReader("12345"), 5); err != nil {
		t.Fatalf("exact-size upload failed: %v", err)
	}
}

func TestMaxInt64Limit(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// maxBytes+1 overflows to a negative number for math.MaxInt64; the limit
	// must not silently truncate the upload to zero bytes.
	entry, err := s.Put(model.KindClipboard, "", model.ContentTypeText, strings.NewReader("abc"), math.MaxInt64)
	if err != nil {
		t.Fatalf("MaxInt64 limit rejected: %v", err)
	}
	if entry.Size != 3 {
		t.Fatalf("size = %d, want 3", entry.Size)
	}
	f, _, err := s.Open(model.KindClipboard)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	if string(data) != "abc" {
		t.Fatalf("content = %q", data)
	}
}

func TestClipboardContentType(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(model.KindClipboard, "", "image/png", strings.NewReader("\x89PNG"), 1024); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := reopened.Get(model.KindClipboard)
	if !ok || entry.ContentType != "image/png" {
		t.Fatalf("content type = %q ok = %v", entry.ContentType, ok)
	}
}

func TestRepairDefaultsClipboardToText(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "clipboard"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := s.Get(model.KindClipboard)
	if !ok || entry.ContentType != model.ContentTypeText {
		t.Fatalf("repaired content type = %q ok = %v", entry.ContentType, ok)
	}
}

func TestZeroByteFile(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Put(model.KindFile, "empty", "", strings.NewReader(""), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Size != 0 {
		t.Fatalf("size = %d", entry.Size)
	}
	if _, ok := s.Get(model.KindFile); !ok {
		t.Fatal("zero-byte file reported as empty registry")
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(model.KindClipboard, "", model.ContentTypeText, strings.NewReader("survives"), 1024); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, entry, err := reopened.Open(model.KindClipboard)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	if string(data) != "survives" || entry.Size != 8 {
		t.Fatalf("content = %q entry = %+v", data, entry)
	}
}

func TestNewFormatManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Put(model.KindFile, "notes.txt", "text/plain", strings.NewReader("hello"), 1024)
	if err != nil {
		t.Fatal(err)
	}

	m, err := s.readManifest(model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	if m.Data == "" {
		t.Fatal("manifest has no data generation")
	}
	if _, err := os.Stat(filepath.Join(dir, m.Data)); err != nil {
		t.Fatalf("referenced generation missing: %v", err)
	}
	if _, err := os.Stat(s.dataPath(model.KindFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new-format write used legacy path: %v", err)
	}
	raw, err := os.ReadFile(s.metaPath(model.KindFile))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["data"]; !ok {
		t.Fatalf("manifest does not record data blob: %s", raw)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, got, err := reopened.Open(model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	if string(data) != "hello" || got.Name != "notes.txt" || got.SHA256 != entry.SHA256 {
		t.Fatalf("content = %q entry = %+v", data, got)
	}
}

func TestLegacyFormatMigration(t *testing.T) {
	dir := t.TempDir()
	content := []byte("legacy")
	sum := sha256.Sum256(content)
	old := model.Entry{
		Name:      "old.txt",
		Size:      int64(len(content)),
		SHA256:    hex.EncodeToString(sum[:]),
		UpdatedAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Second),
	}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, model.KindFile), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, model.KindFile+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := s.Get(model.KindFile)
	if !ok || entry.Name != "old.txt" || entry.Size != 6 || entry.SHA256 != old.SHA256 {
		t.Fatalf("legacy entry = %+v ok = %v", entry, ok)
	}
	f, _, err := s.Open(model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(f)
	f.Close()
	if string(data) != "legacy" {
		t.Fatalf("legacy content = %q", data)
	}

	if _, err := s.Put(model.KindFile, "new.txt", "text/plain", strings.NewReader("modern"), 1024); err != nil {
		t.Fatal(err)
	}
	m, err := s.readManifest(model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	if m.Data == "" {
		t.Fatal("put did not migrate to a generation blob")
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.dataPath(model.KindFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy data survived migration: %v", err)
	}
	f, got, err := reopened.Open(model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, _ = io.ReadAll(f)
	if string(data) != "modern" || got.Name != "new.txt" || got.ContentType != "text/plain" {
		t.Fatalf("content = %q entry = %+v", data, got)
	}
}

func TestSameSizeOverwriteRefreshesMetadata(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Put(model.KindFile, "old.txt", "text/plain", strings.NewReader("abcd"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Put(model.KindFile, "new.txt", "application/octet-stream", strings.NewReader("wxyz"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 == second.SHA256 {
		t.Fatal("same-size overwrite kept stale sha256")
	}
	if second.Name != "new.txt" || second.ContentType != "application/octet-stream" || second.Size != 4 {
		t.Fatalf("unexpected entry: %+v", second)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := reopened.Get(model.KindFile)
	if !ok || entry.Name != "new.txt" || entry.ContentType != "application/octet-stream" || entry.SHA256 != second.SHA256 {
		t.Fatalf("stale metadata after reopen: %+v ok = %v", entry, ok)
	}
	f, _, err := reopened.Open(model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	if string(data) != "wxyz" {
		t.Fatalf("content = %q", data)
	}
}

func TestRepairRemovesOrphanBlobsAndTemps(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(model.KindFile, "keep.txt", "", strings.NewReader("keep"), 1024); err != nil {
		t.Fatal(err)
	}
	m, err := s.readManifest(model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	referenced := filepath.Join(dir, m.Data)
	orphan := filepath.Join(dir, model.KindFile+".deadbeef.data")
	temp := filepath.Join(dir, model.KindFile+".tmp-crashed")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, []byte("temp"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan blob survived repair: %v", err)
	}
	if _, err := os.Stat(temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file survived repair: %v", err)
	}
	if _, err := os.Stat(referenced); err != nil {
		t.Fatalf("referenced blob removed: %v", err)
	}
	f, _, err := reopened.Open(model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	if string(data) != "keep" {
		t.Fatalf("content = %q", data)
	}
}

func TestRepairMissingMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, model.KindFile), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := reopened.Get(model.KindFile)
	if !ok || entry.Size != 3 || entry.Name != "file" {
		t.Fatalf("repaired entry = %+v ok = %v", entry, ok)
	}
	f, _, err := reopened.Open(model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	if string(data) != "abc" {
		t.Fatalf("content = %q", data)
	}
}

func TestRepairDropsMetadataWithoutData(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(model.KindClipboard, "", model.ContentTypeText, strings.NewReader("gone"), 1024); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(currentDataPath(t, s, model.KindClipboard)); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get(model.KindClipboard); ok {
		t.Fatal("stale metadata survived repair")
	}
}

func TestRepairDoesNotAdoptOrphanGeneration(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(model.KindFile, "lost.txt", "", strings.NewReader("lost"), 1024); err != nil {
		t.Fatal(err)
	}
	m, err := s.readManifest(model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(dir, m.Data)
	if err := os.Remove(s.metaPath(model.KindFile)); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get(model.KindFile); ok {
		t.Fatal("uncommitted generation was adopted")
	}
	if _, err := os.Stat(blob); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan generation survived repair: %v", err)
	}
}

func TestInvalidKind(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("nope"); ok {
		t.Fatal("unknown kind returned metadata")
	}
	if _, _, err := s.Open("nope"); err == nil {
		t.Fatal("unknown kind opened")
	}
	if _, err := s.Put("nope", "", "", strings.NewReader("x"), 10); err == nil {
		t.Fatal("unknown kind stored")
	}
}

func TestRejectsNonPositiveLimit(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(model.KindClipboard, "", model.ContentTypeText, strings.NewReader("x"), 0); err == nil {
		t.Fatal("zero limit accepted")
	}
}

func TestPutDoesNotBlockReadsWhileStreaming(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(model.KindFile, "before.txt", "", strings.NewReader("before"), 1024); err != nil {
		t.Fatal(err)
	}

	blocking := newBlockingReader("after")
	released := false
	release := func() {
		if !released {
			released = true
			close(blocking.release)
		}
	}
	defer release()

	putDone := make(chan error, 1)
	go func() {
		_, err := s.Put(model.KindFile, "after.txt", "", blocking, 1024)
		putDone <- err
	}()

	select {
	case <-blocking.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Put did not start reading the body")
	}

	type getResult struct {
		entry model.Entry
		ok    bool
	}
	getDone := make(chan getResult, 1)
	go func() {
		entry, ok := s.Get(model.KindFile)
		getDone <- getResult{entry: entry, ok: ok}
	}()
	select {
	case got := <-getDone:
		if !got.ok || got.entry.Name != "before.txt" {
			t.Fatalf("Get during upload = %+v ok = %v", got.entry, got.ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Get blocked while an upload was streaming")
	}

	openDone := make(chan error, 1)
	go func() {
		f, entry, err := s.Open(model.KindFile)
		if err != nil {
			openDone <- err
			return
		}
		defer f.Close()
		data, err := io.ReadAll(f)
		if err != nil {
			openDone <- err
			return
		}
		if string(data) != "before" || entry.Name != "before.txt" {
			openDone <- fmt.Errorf("Open during upload = %q %+v", data, entry)
			return
		}
		openDone <- nil
	}()
	select {
	case err := <-openDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open blocked while an upload was streaming")
	}

	release()
	if err := <-putDone; err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	f, entry, err := s.Open(model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	if string(data) != "after" || entry.Name != "after.txt" {
		t.Fatalf("content after upload = %q entry = %+v", data, entry)
	}
}

func currentDataPath(t *testing.T, s *Store, kind string) string {
	t.Helper()
	m, err := s.readManifest(kind)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if m.Data != "" {
		return filepath.Join(s.dir, m.Data)
	}
	return s.dataPath(kind)
}

// blockingReader signals when Put starts reading and then waits until release
// is closed before returning its payload.
type blockingReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	data    string
	sent    bool
}

func newBlockingReader(data string) *blockingReader {
	return &blockingReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
		data:    data,
	}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	if r.sent {
		return 0, io.EOF
	}
	r.sent = true
	return copy(p, r.data), nil
}
