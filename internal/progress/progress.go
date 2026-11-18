// Package progress renders the progress of a transfer on standard error.
//
// A Bar is an io.Writer. Wrap the reader or writer of a transfer with it and
// the bar counts the bytes that pass through. On a terminal it redraws one
// line at most every 80 ms; when stderr is not a terminal it stays quiet until
// Done, which prints a single summary line. Nothing is written to stdout, so
// piped data stays clean.
package progress

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	barWidth    = 20
	redrawEvery = 80 * time.Millisecond
)

// Bar renders the progress of one transfer. The zero value is not usable; call
// New.
type Bar struct {
	label string
	total int64
	n     int64

	start    time.Time
	lastDraw time.Time
	lastLen  int

	tty  bool
	done bool
	mu   sync.Mutex
}

// New returns a bar for label. A total <= 0 means the size is unknown; the bar
// then shows only the amount transferred.
func New(label string, total int64) *Bar {
	if total < 0 {
		total = 0
	}
	fi, err := os.Stderr.Stat()
	return &Bar{
		label: cleanLabel(label),
		total: total,
		start: time.Now(),
		tty:   err == nil && fi.Mode()&os.ModeCharDevice != 0 && os.Getenv("TERM") != "dumb",
	}
}

// SetTotal updates the expected size, for example once a Content-Length is
// known. A value <= 0 means unknown.
func (b *Bar) SetTotal(total int64) {
	if total < 0 {
		total = 0
	}
	b.mu.Lock()
	b.total = total
	b.mu.Unlock()
}

// Write counts p and redraws the bar when enough time has passed.
func (b *Bar) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.n += int64(len(p))
	b.drawLocked()
	b.mu.Unlock()
	return len(p), nil
}

// Done ends the bar and prints its final state. It is idempotent.
func (b *Bar) Done() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return
	}
	b.done = true
	if b.tty {
		fmt.Fprintf(os.Stderr, "\r%s\n", b.paddedLocked(b.lineLocked()))
		return
	}
	fmt.Fprintf(os.Stderr, "%s: %s in %.1fs (%s/s)\n",
		b.label,
		HumanSize(b.n),
		b.elapsedLocked().Seconds(),
		HumanSize(int64(b.rateLocked())),
	)
}

// HumanSize formats n with 1024-based units, e.g. "12 B", "1.5 KB", "4.4 MB".
func HumanSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := [...]string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	for i, unit := range units {
		v /= 1024
		if v < 1024 || i == len(units)-1 {
			return fmt.Sprintf("%.1f %s", v, unit)
		}
	}
	return fmt.Sprintf("%d B", n)
}

func (b *Bar) drawLocked() {
	if !b.tty || b.done {
		return
	}
	if !b.lastDraw.IsZero() && time.Since(b.lastDraw) < redrawEvery {
		return
	}
	fmt.Fprintf(os.Stderr, "\r%s", b.paddedLocked(b.lineLocked()))
	b.lastDraw = time.Now()
}

// paddedLocked pads line with spaces so it covers the previous, longer line.
func (b *Bar) paddedLocked(line string) string {
	width := len(line)
	if width < b.lastLen {
		line += strings.Repeat(" ", b.lastLen-width)
		width = b.lastLen
	}
	b.lastLen = width
	return line
}

func (b *Bar) lineLocked() string {
	var sb strings.Builder
	sb.WriteString(b.label)
	sb.WriteByte(' ')
	if b.total > 0 {
		frac := float64(b.n) / float64(b.total)
		if frac > 1 {
			frac = 1
		}
		filled := int(frac*barWidth + 0.5)
		sb.WriteByte('[')
		sb.WriteString(strings.Repeat("=", filled))
		sb.WriteString(strings.Repeat("-", barWidth-filled))
		sb.WriteString("] ")
		fmt.Fprintf(&sb, "%3d%%  %s / %s", int(frac*100), HumanSize(b.n), HumanSize(b.total))
	} else {
		sb.WriteString(HumanSize(b.n))
	}
	rate := b.rateLocked()
	if rate > 0 {
		fmt.Fprintf(&sb, "  %s/s", HumanSize(int64(rate)))
	}
	if b.total > 0 && b.n < b.total && rate > 0 {
		left := time.Duration(float64(b.total-b.n) / rate * float64(time.Second))
		fmt.Fprintf(&sb, "  ETA %s", left.Round(time.Second))
	}
	return sb.String()
}

func (b *Bar) elapsedLocked() time.Duration { return time.Since(b.start) }

func (b *Bar) rateLocked() float64 {
	secs := b.elapsedLocked().Seconds()
	if secs < 0.001 {
		secs = 0.001
	}
	return float64(b.n) / secs
}

// cleanLabel hides control characters so terminal escape sequences in a file
// name cannot corrupt the display.
func cleanLabel(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}
