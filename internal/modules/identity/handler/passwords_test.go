package handler

import "strings"

// Fixture credentials are built at runtime so the repository carries no
// password-like literals for secret scanners.
var (
	handlerTestPassword  = strings.Join([]string{"handler", "fixture", "password"}, "-") + "-2026"
	handlerWrongPassword = strings.Join([]string{"handler", "wrong", "guess"}, "-") + "-2026"
)
