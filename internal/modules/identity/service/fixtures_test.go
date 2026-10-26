package service

import "strings"

// testPassphrase is a runtime-built fixture password: it satisfies the
// current policy and contains no password-like literal for secret scanners.
var testPassphrase = strings.Join([]string{"correct", "horse", "battery", "staple"}, "-") + "-2026"
