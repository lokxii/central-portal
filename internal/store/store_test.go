package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestRepairMissingMetadata(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(model.KindFile, "notes.txt", "", strings.NewReader("abc"), 1024); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.metaPath(model.KindFile)); err != nil {
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
	if err := os.Remove(s.dataPath(model.KindClipboard)); err != nil {
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
