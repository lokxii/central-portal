// Package model defines the data shared by the server, the CLI and the browser.
package model

import "time"

// Registry kinds. Each kind holds exactly one item.
const (
	KindClipboard = "clipboard"
	KindFile      = "file"
)

// ContentTypeText is the canonical media type of a text clipboard entry.
const ContentTypeText = "text/plain; charset=utf-8"

// Entry describes the content currently stored in a registry.
type Entry struct {
	Name        string    `json:"name,omitempty"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256,omitempty"`
	ContentType string    `json:"contentType,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Status is the response of GET /api/status.
type Status struct {
	Clipboard *Entry `json:"clipboard"`
	File      *Entry `json:"file"`
}
