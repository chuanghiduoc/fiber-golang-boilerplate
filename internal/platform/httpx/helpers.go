// Package httpx holds framework-neutral HTTP header helpers shared by module
// handlers.
package httpx

import (
	"fmt"
	"strings"
)

// ContentDispositionAttachment builds an RFC 6266 Content-Disposition header
// for a downloadable file. It emits an ASCII-safe filename="" plus an RFC 5987
// filename*= for Unicode, and strips control/path characters from the
// client-supplied name to prevent header injection and path confusion.
func ContentDispositionAttachment(name string) string {
	// Use only the base name and drop any directory components. Names made only
	// of spaces and dots are not useful as downloads and can confuse clients.
	name = name[strings.LastIndexAny(name, "/\\")+1:]
	if strings.Trim(name, " .") == "" {
		name = "download"
	}

	var ascii strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f: // control chars (incl. CR/LF)
			continue
		case r == '"' || r == '\\':
			ascii.WriteByte('_')
		case r > 0x7f: // non-ASCII → placeholder in the fallback token
			ascii.WriteByte('_')
		default:
			ascii.WriteRune(r)
		}
	}
	fallback := ascii.String()
	if fallback == "" {
		fallback = "download"
	}

	// filename* carries the exact UTF-8 name for clients that support RFC 5987.
	// PathEscape is not an RFC 5987 encoder: it leaves characters such as '@'
	// and ':' unescaped even though they are not attr-chars.
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", fallback, encodeRFC5987(name))
}

// encodeRFC5987 percent-encodes every byte that is not an RFC 5987 attr-char.
// It is deliberately byte-oriented so UTF-8 filenames arrive intact.
func encodeRFC5987(value string) string {
	const hex = "0123456789ABCDEF"
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if isRFC5987AttrChar(c) {
			out.WriteByte(c)
			continue
		}
		out.WriteByte('%')
		out.WriteByte(hex[c>>4])
		out.WriteByte(hex[c&0x0f])
	}
	return out.String()
}

func isRFC5987AttrChar(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '!', '#', '$', '&', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}
