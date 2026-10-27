// Package httpx holds framework-neutral HTTP header helpers shared by module
// handlers.
package httpx

import (
	"fmt"
	"net/url"
	"strings"
)

// ContentDispositionAttachment builds an RFC 6266 Content-Disposition header
// for a downloadable file. It emits an ASCII-safe filename="" plus an RFC 5987
// filename*= for Unicode, and strips control/path characters from the
// client-supplied name to prevent header injection and path confusion.
func ContentDispositionAttachment(name string) string {
	// Use only the base name and drop any directory components.
	name = name[strings.LastIndexAny(name, "/\\")+1:]

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
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", fallback, url.PathEscape(name))
}
