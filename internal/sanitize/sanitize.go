// Package sanitize makes untrusted text safe for terminal output.
//
// Names and error messages can come from the server or from a file name. A
// terminal interprets control characters, so an uploader could otherwise inject
// OSC/CSI sequences (title changes, clipboard writes) or spoof lines.
package sanitize

import "strings"

// Clean replaces control and formatting characters with '?'. It keeps ordinary
// printable Unicode, including non-ASCII names.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			return '?'
		case r == 0x202a || r == 0x202b || r == 0x202c || r == 0x202d || r == 0x202e:
			return '?'
		case r >= 0x2066 && r <= 0x2069:
			return '?'
		case r == 0x200b || r == 0x200c || r == 0x200d || r == 0xfeff:
			return '?'
		}
		return r
	}, s)
}
