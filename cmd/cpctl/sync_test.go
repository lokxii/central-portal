package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"central-portal/internal/client"
	"central-portal/internal/model"
)

type fakePortal struct {
	status      model.Status
	clip        []byte
	contentType string
	puts        [][]byte
	putErr      error
}

func (f *fakePortal) Status(context.Context) (model.Status, error) { return f.status, nil }

func (f *fakePortal) GetClipboard(context.Context, io.Writer) ([]byte, string, error) {
	if f.status.Clipboard == nil {
		return nil, "", client.ErrEmpty
	}
	return append([]byte(nil), f.clip...), f.contentType, nil
}

func (f *fakePortal) PutClipboard(_ context.Context, data []byte, contentType string, _ io.Writer) (model.Entry, error) {
	if f.putErr != nil {
		return model.Entry{}, f.putErr
	}
	f.puts = append(f.puts, append([]byte(nil), data...))
	f.clip = append([]byte(nil), data...)
	f.contentType = contentType
	e := model.Entry{
		Size:        int64(len(data)),
		SHA256:      hashBytes(data),
		ContentType: contentType,
		UpdatedAt:   time.Now().UTC(),
	}
	f.status.Clipboard = &e
	return e, nil
}

func (f *fakePortal) StreamEvents(context.Context, func(client.Event) error) error { return nil }

type fakeClipboard struct {
	data        []byte
	contentType string
	writes      int
}

func (c *fakeClipboard) read(context.Context) ([]byte, string, error) {
	if c.data == nil {
		return nil, "", errors.New("wayland clipboard has no text or image content")
	}
	return append([]byte(nil), c.data...), c.contentType, nil
}

func (c *fakeClipboard) write(_ context.Context, data []byte, contentType string) error {
	c.data = append([]byte(nil), data...)
	c.contentType = contentType
	c.writes++
	return nil
}

func newTestSyncer(p clipboardClient, cb *fakeClipboard) *syncer {
	return &syncer{
		client: p,
		read:   cb.read,
		write:  cb.write,
		logf:   func(string, ...any) {},
	}
}

func TestSyncerAppliesRemoteEntry(t *testing.T) {
	payload := []byte("from the server")
	hash := hashBytes(payload)
	portal := &fakePortal{
		status:      model.Status{Clipboard: &model.Entry{Size: int64(len(payload)), SHA256: hash, ContentType: model.ContentTypeText}},
		clip:        payload,
		contentType: model.ContentTypeText,
	}
	cb := &fakeClipboard{}
	s := newTestSyncer(portal, cb)

	if err := s.handleRemote(context.Background(), portal.status.Clipboard); err != nil {
		t.Fatalf("handleRemote: %v", err)
	}
	if cb.writes != 1 || string(cb.data) != string(payload) {
		t.Fatalf("clipboard write = %d %q, want the server payload", cb.writes, cb.data)
	}
	if s.lastRemote != hash || s.lastLocal != hash {
		t.Fatalf("state = (%q, %q), want both %q", s.lastRemote, s.lastLocal, hash)
	}
}

func TestSyncerPushesLocalChangeAndIgnoresEcho(t *testing.T) {
	payload := []byte("from the laptop")
	hash := hashBytes(payload)
	portal := &fakePortal{}
	cb := &fakeClipboard{data: payload, contentType: model.ContentTypeText}
	s := newTestSyncer(portal, cb)
	s.ready = true

	if err := s.handleLocal(context.Background(), localClip{data: payload, contentType: model.ContentTypeText, hash: hash}); err != nil {
		t.Fatalf("handleLocal: %v", err)
	}
	if len(portal.puts) != 1 || string(portal.puts[0]) != string(payload) {
		t.Fatalf("uploads = %q, want one local payload", portal.puts)
	}

	// The server's broadcast of our own upload must not be written back.
	if err := s.handleRemote(context.Background(), portal.status.Clipboard); err != nil {
		t.Fatalf("handleRemote echo: %v", err)
	}
	if cb.writes != 0 {
		t.Fatalf("clipboard writes = %d, want 0 for an echo", cb.writes)
	}
}

func TestSyncerHoldsLocalChangeUntilServerStateKnown(t *testing.T) {
	payload := []byte("early local")
	portal := &fakePortal{}
	cb := &fakeClipboard{data: payload, contentType: model.ContentTypeText}
	s := newTestSyncer(portal, cb)

	if err := s.handleLocal(context.Background(), localClip{data: payload, contentType: model.ContentTypeText, hash: hashBytes(payload)}); err != nil {
		t.Fatalf("handleLocal: %v", err)
	}
	if len(portal.puts) != 0 {
		t.Fatal("uploaded before the server state was known")
	}
	if s.pending == nil {
		t.Fatal("local change was not held back")
	}

	// An empty registry snapshot releases it.
	if err := s.handleRemote(context.Background(), nil); err != nil {
		t.Fatalf("handleRemote: %v", err)
	}
	if len(portal.puts) != 1 {
		t.Fatalf("uploads = %d, want the held local change", len(portal.puts))
	}
}

func TestSyncerRemoteEntrySupersedesHeldLocalChange(t *testing.T) {
	local := []byte("offline local")
	remote := []byte("server wins")
	hash := hashBytes(remote)
	portal := &fakePortal{
		status:      model.Status{Clipboard: &model.Entry{Size: int64(len(remote)), SHA256: hash, ContentType: model.ContentTypeText}},
		clip:        remote,
		contentType: model.ContentTypeText,
	}
	cb := &fakeClipboard{data: local, contentType: model.ContentTypeText}
	s := newTestSyncer(portal, cb)

	if err := s.handleLocal(context.Background(), localClip{data: local, contentType: model.ContentTypeText, hash: hashBytes(local)}); err != nil {
		t.Fatalf("handleLocal: %v", err)
	}
	if err := s.handleRemote(context.Background(), portal.status.Clipboard); err != nil {
		t.Fatalf("handleRemote: %v", err)
	}
	if s.pending != nil {
		t.Fatal("held local change survived a newer server entry")
	}
	if err := s.handleRemote(context.Background(), portal.status.Clipboard); err != nil {
		t.Fatalf("handleRemote repeat: %v", err)
	}
	if len(portal.puts) != 0 {
		t.Fatalf("uploads = %d, want none after the server entry won", len(portal.puts))
	}
}

func TestSyncerIgnoresOlderRemoteEntry(t *testing.T) {
	oldData := []byte("old server value")
	newData := []byte("newer server value")
	newTime := time.Now().UTC()
	portal := &fakePortal{clip: oldData, contentType: model.ContentTypeText}
	cb := &fakeClipboard{data: newData, contentType: model.ContentTypeText}
	s := newTestSyncer(portal, cb)
	s.ready = true
	s.lastRemote = hashBytes(newData)
	s.lastRemoteTime = newTime
	s.lastLocal = hashBytes(newData)

	older := &model.Entry{
		Size:        int64(len(oldData)),
		SHA256:      hashBytes(oldData),
		ContentType: model.ContentTypeText,
		UpdatedAt:   newTime.Add(-time.Minute),
	}
	if err := s.handleRemote(context.Background(), older); err != nil {
		t.Fatalf("handleRemote: %v", err)
	}
	if cb.writes != 0 || s.lastRemote != hashBytes(newData) {
		t.Fatalf("older entry was applied: writes=%d lastRemote=%q", cb.writes, s.lastRemote)
	}
}

func TestSyncerIgnoresStaleClipboardObservation(t *testing.T) {
	payload := []byte("read before a remote apply")
	portal := &fakePortal{}
	cb := &fakeClipboard{data: payload, contentType: model.ContentTypeText}
	s := newTestSyncer(portal, cb)
	s.ready = true
	s.epoch = 1

	// Epoch 0 was read before the remote apply; it must not be uploaded.
	if err := s.handleLocal(context.Background(), localClip{data: payload, contentType: model.ContentTypeText, hash: hashBytes(payload), epoch: 0}); err != nil {
		t.Fatalf("handleLocal: %v", err)
	}
	if len(portal.puts) != 0 {
		t.Fatalf("uploads = %d, want a stale observation ignored", len(portal.puts))
	}
}

func TestSyncerRetriesFailedUploadOnReconcile(t *testing.T) {
	payload := []byte("retry me")
	portal := &fakePortal{putErr: errors.New("network down")}
	cb := &fakeClipboard{data: payload, contentType: model.ContentTypeText}
	s := newTestSyncer(portal, cb)
	s.ready = true

	if err := s.handleLocal(context.Background(), localClip{data: payload, contentType: model.ContentTypeText, hash: hashBytes(payload)}); err == nil {
		t.Fatal("handleLocal hid the upload failure")
	}
	if s.pending == nil {
		t.Fatal("failed upload was not kept for retry")
	}

	portal.putErr = nil
	if err := s.handleRemote(context.Background(), nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(portal.puts) != 1 {
		t.Fatalf("uploads = %d, want one retry", len(portal.puts))
	}
}
