// Package web embeds the browser application.
package web

import "embed"

// Files holds the static frontend rooted at web/static.
//
//go:embed static
var Files embed.FS
