package handler

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
)

// HTTP helpers shared across handlers live in platform/httpx. This unexported
// wrapper keeps the module's call site stable and the locale-independent header
// format testable in one place.

func contentDispositionAttachment(name string) string {
	return httpx.ContentDispositionAttachment(name)
}
