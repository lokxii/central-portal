// Bidirectional clipboard sync: local clipboard changes are uploaded and
// server changes are written back to the Wayland clipboard. It replaces the
// one-shot clip push and clip pull commands.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"central-portal/internal/client"
	"central-portal/internal/model"
	"central-portal/internal/progress"
	"central-portal/internal/sanitize"
)

const (
	usageClipSync = "usage: cpctl clip sync [-interval DURATION] [-reconcile DURATION] [-once] [-server URL] [-ca PATH]\n"

	defaultSyncInterval      = time.Second
	defaultReconcileInterval = 30 * time.Second
	syncOpTimeout            = 30 * time.Second
	sseLifetime              = 5 * time.Minute
	sseMaxBackoff            = 30 * time.Second
)

// localClip is one observation of the local Wayland clipboard. epoch ties it to
// the clipboard state it was read from, so an observation taken while a remote
// entry was being applied cannot be mistaken for a new local change.
type localClip struct {
	data        []byte
	contentType string
	hash        string
	epoch       uint64
	err         error
}

// clipboardClient is the subset of *client.Client the syncer needs, kept as an
// interface so the state machine can be tested without a server.
type clipboardClient interface {
	Status(context.Context) (model.Status, error)
	GetClipboard(context.Context, io.Writer) ([]byte, string, error)
	PutClipboard(context.Context, []byte, string, io.Writer) (model.Entry, error)
	StreamEvents(context.Context, func(client.Event) error) error
}

// syncer keeps one laptop's clipboard and one portal registry in step. All
// state is owned by run's event loop; the poller and stream goroutines only
// deliver messages.
type syncer struct {
	client    clipboardClient
	read      func(context.Context) ([]byte, string, error)
	write     func(context.Context, []byte, string) error
	interval  time.Duration
	reconcile time.Duration
	logf      func(format string, args ...any)

	clipMu sync.Mutex // serializes clipboard reads and writes
	epoch  uint64     // incremented whenever a remote entry replaces the clipboard

	ready          bool      // the server's current entry has been observed
	lastRemote     string    // SHA-256 of the entry currently in the registry
	lastRemoteTime time.Time // UpdatedAt of lastRemote, to discard stale snapshots
	lastLocal      string    // SHA-256 of the local clipboard after the last apply/push
	pending        *localClip
	localErr       string
}

func clipSync(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cpctl clip sync", flag.ContinueOnError)
	server := fs.String("server", defaultServer(), "portal base URL")
	ca := fs.String("ca", defaultCAFile(), "portal CA certificate; empty uses system roots")
	interval := fs.Duration("interval", defaultSyncInterval, "how often to check the local clipboard")
	reconcile := fs.Duration("reconcile", defaultReconcileInterval, "how often to reconcile with /api/status")
	once := fs.Bool("once", false, "reconcile once and exit instead of running in the background")
	pos, err := parseFlags(fs, args, stdout, usageClipSync)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errUsage
	}
	if *interval < 100*time.Millisecond {
		return errors.New("-interval must be at least 100ms")
	}
	if *reconcile < *interval {
		return errors.New("-reconcile must not be shorter than -interval")
	}

	// Fail before any network traffic when the clipboard cannot be used.
	if err := requireWayland("wl-paste", "wl-copy"); err != nil {
		return err
	}
	c, err := newClient(*server, *ca)
	if err != nil {
		return err
	}

	s := &syncer{
		client:    c,
		read:      readWaylandClipboard,
		write:     writeWaylandClipboard,
		interval:  *interval,
		reconcile: *reconcile,
		logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "cpctl: "+format+"\n", args...)
		},
	}
	if *once {
		return s.once(ctx)
	}
	s.logf("watching the Wayland clipboard and %s", sanitize.Clean(*server))
	return s.run(ctx)
}

// run reconciles once, then handles local and remote changes until ctx ends.
func (s *syncer) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := s.initialSync(ctx); err != nil {
		s.logf("initial sync: %v (will keep trying)", err)
	}

	localCh := make(chan localClip, 4)
	remoteCh := make(chan *model.Entry, 4)
	go s.pollLocal(ctx, localCh)
	go s.streamRemote(ctx, remoteCh)
	go s.reconcileLoop(ctx, remoteCh)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case lc := <-localCh:
			if err := s.handleLocal(ctx, lc); err != nil {
				s.logf("push clipboard: %v", err)
			}
		case entry := <-remoteCh:
			if err := s.handleRemote(ctx, entry); err != nil {
				s.logf("pull clipboard: %v", err)
			}
		}
	}
}

// once performs one reconciliation. It is useful after a failed daemon start
// and as a replacement for the old one-shot commands.
func (s *syncer) once(ctx context.Context) error {
	st, err := s.status(ctx)
	if err != nil {
		return err
	}
	if st.Clipboard != nil {
		return s.handleRemote(ctx, st.Clipboard)
	}
	s.ready = true
	lc, err := s.readLocal(ctx)
	if err != nil {
		return err
	}
	return s.handleLocal(ctx, lc)
}

func (s *syncer) initialSync(ctx context.Context) error {
	st, err := s.status(ctx)
	if err != nil {
		return err
	}
	if st.Clipboard != nil {
		return s.handleRemote(ctx, st.Clipboard)
	}
	// An empty registry adopts whatever is on the clipboard already.
	s.ready = true
	local, err := s.readLocal(ctx)
	if err != nil {
		s.reportLocalErr(err)
		return nil
	}
	return s.handleLocal(ctx, local)
}

// pollLocal reads the Wayland clipboard on an interval and reports changes.
func (s *syncer) pollLocal(ctx context.Context, out chan<- localClip) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	last := ""
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		s.clipMu.Lock()
		epoch := s.epoch
		data, contentType, err := s.read(ctx)
		s.clipMu.Unlock()
		if err != nil {
			failing = true
			out <- localClip{err: err}
		} else {
			hash := hashBytes(data)
			if hash != last || failing {
				last = hash
				failing = false
				out <- localClip{data: data, contentType: contentType, hash: hash, epoch: epoch}
			}
		}
		timer.Reset(s.interval)
	}
}

// streamRemote follows /api/events, reconnecting with backoff. Every connection
// starts with the current entry, so a reconnect also repairs missed updates.
func (s *syncer) streamRemote(ctx context.Context, out chan<- *model.Entry) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		streamCtx, cancel := context.WithTimeout(ctx, sseLifetime)
		err := s.client.StreamEvents(streamCtx, func(ev client.Event) error {
			if ev.Kind != model.KindClipboard {
				return nil
			}
			select {
			case out <- ev.Entry:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		rotated := errors.Is(streamCtx.Err(), context.DeadlineExceeded)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if rotated {
			// A quiet stream is recycled so a silently dead connection cannot
			// linger forever.
			backoff = time.Second
			continue
		}
		// A stream that stayed up for a while earns a fresh delay.
		if time.Since(start) > 10*time.Second {
			backoff = time.Second
		}
		s.logf("event stream: %v (reconnecting in %s)", err, backoff)

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff *= 2
		if backoff > sseMaxBackoff {
			backoff = sseMaxBackoff
		}
	}
}

// reconcileLoop periodically compares against /api/status. It is a fallback for
// a silently broken event stream and retries pending uploads.
func (s *syncer) reconcileLoop(ctx context.Context, out chan<- *model.Entry) {
	ticker := time.NewTicker(s.reconcile)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st, err := s.status(ctx)
			if err != nil {
				s.logf("reconcile: %v", err)
				continue
			}
			select {
			case out <- st.Clipboard:
			case <-ctx.Done():
				return
			}
		}
	}
}

// handleLocal uploads a changed local clipboard unless it is already what the
// registry holds. Before the server state is known, the latest local value is
// held back so a late connection cannot overwrite newer server content.
func (s *syncer) handleLocal(ctx context.Context, lc localClip) error {
	if lc.err != nil {
		s.reportLocalErr(lc.err)
		return nil
	}
	s.reportLocalErr(nil)
	if lc.hash == "" || len(lc.data) == 0 {
		return nil
	}
	if lc.epoch != s.epoch {
		// Read before a remote entry replaced the clipboard; not a user change.
		return nil
	}
	if !s.ready {
		s.pending = &lc
		return nil
	}
	if lc.hash == s.lastLocal || lc.hash == s.lastRemote {
		s.pending = nil
		return nil
	}
	if err := s.pushLocal(ctx, lc); err != nil {
		s.pending = &lc
		return err
	}
	s.pending = nil
	return nil
}

// handleRemote applies a registry entry to the local clipboard. Entries that
// match the local clipboard are acknowledged without writing, which breaks the
// echo loop between a local upload and its own broadcast.
func (s *syncer) handleRemote(ctx context.Context, entry *model.Entry) error {
	s.ready = true
	if entry == nil || entry.SHA256 == "" {
		s.lastRemote = ""
		s.lastRemoteTime = time.Time{}
		return s.flushPending(ctx)
	}
	if !s.lastRemoteTime.IsZero() && entry.UpdatedAt.Before(s.lastRemoteTime) {
		// A status request that raced an upload can deliver an older snapshot
		// after a newer event; never go backwards.
		return nil
	}
	if entry.SHA256 != s.lastRemote {
		// A different non-empty server entry wins over local content that has
		// not reached the registry yet.
		s.pending = nil
	}
	if entry.SHA256 == s.lastRemote {
		s.lastRemoteTime = entry.UpdatedAt
		return s.flushPending(ctx)
	}
	if entry.SHA256 == s.lastLocal {
		s.lastRemote = entry.SHA256
		s.lastRemoteTime = entry.UpdatedAt
		return nil
	}

	data, contentType, err := s.getClipboard(ctx)
	if err != nil {
		return err
	}
	if hashBytes(data) != entry.SHA256 {
		// The registry changed again while downloading; its newer event wins.
		return nil
	}
	s.clipMu.Lock()
	defer s.clipMu.Unlock()
	s.epoch++
	if err := s.write(ctx, data, contentType); err != nil {
		return err
	}
	s.lastRemote = entry.SHA256
	s.lastRemoteTime = entry.UpdatedAt
	s.lastLocal = entry.SHA256
	// Wayland may normalize text, so record what reading it back yields.
	if local, _, err := s.read(ctx); err == nil {
		s.lastLocal = hashBytes(local)
	}
	s.logf("pulled %s %s", progress.HumanSize(int64(len(data))), sanitize.Clean(contentType))
	return nil
}

// flushPending uploads local content that arrived before the server state was
// known, or whose previous upload failed. It only runs when the registry is
// empty or unchanged, so it never overwrites a newer remote entry.
func (s *syncer) flushPending(ctx context.Context) error {
	if s.pending == nil {
		return nil
	}
	lc := *s.pending
	s.pending = nil
	if lc.hash == s.lastRemote {
		return nil
	}
	if err := s.pushLocal(ctx, lc); err != nil {
		s.pending = &lc
		return err
	}
	return nil
}

func (s *syncer) pushLocal(ctx context.Context, lc localClip) error {
	ctx, cancel := context.WithTimeout(ctx, syncOpTimeout)
	defer cancel()
	entry, err := s.client.PutClipboard(ctx, lc.data, lc.contentType, nil)
	if err != nil {
		return err
	}
	s.lastLocal = lc.hash
	s.lastRemote = entry.SHA256
	s.lastRemoteTime = entry.UpdatedAt
	s.logf("pushed %s %s", progress.HumanSize(int64(len(lc.data))), sanitize.Clean(lc.contentType))
	return nil
}

func (s *syncer) status(ctx context.Context) (model.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, syncOpTimeout)
	defer cancel()
	return s.client.Status(ctx)
}

func (s *syncer) getClipboard(ctx context.Context) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, syncOpTimeout)
	defer cancel()
	return s.client.GetClipboard(ctx, nil)
}

func (s *syncer) readLocal(ctx context.Context) (localClip, error) {
	s.clipMu.Lock()
	defer s.clipMu.Unlock()
	data, contentType, err := s.read(ctx)
	if err != nil {
		return localClip{}, err
	}
	return localClip{data: data, contentType: contentType, hash: hashBytes(data), epoch: s.epoch}, nil
}

// reportLocalErr logs clipboard read failures once per distinct message instead
// of once per poll tick.
func (s *syncer) reportLocalErr(err error) {
	msg := ""
	if err != nil {
		msg = sanitize.Clean(err.Error())
	}
	if msg == s.localErr {
		return
	}
	s.localErr = msg
	if msg == "" {
		s.logf("local clipboard available again")
		return
	}
	s.logf("local clipboard: %s", msg)
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
