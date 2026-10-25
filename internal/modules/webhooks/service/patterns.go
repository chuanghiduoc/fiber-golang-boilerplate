package service

import "strings"

// maxPatternLen bounds an event type or subscription pattern (the
// webhook_endpoint_subscriptions.pattern and webhook_events.event_type width).
const maxPatternLen = 128

// Matches reports whether eventType matches any of patterns. Patterns are
// exact event names, a trailing-wildcard prefix such as "file.*", or "*".
func Matches(patterns []string, eventType string) bool {
	for _, pattern := range patterns {
		if Match(pattern, eventType) {
			return true
		}
	}
	return false
}

// Match reports whether a single pattern matches eventType.
func Match(pattern, eventType string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "*" {
		return eventType != ""
	}
	if pattern == eventType {
		return eventType != ""
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return eventType != "" && strings.HasPrefix(eventType, prefix) && len(eventType) > len(prefix)
	}
	return false
}

// ValidType reports whether s is a stable lowercase event/pattern name.
// Wildcard patterns "file.*" and "*" are accepted in addition to full names.
func ValidType(s string) bool {
	if s == "*" {
		return true
	}
	if s == "" || len(s) > maxPatternLen {
		return false
	}
	body := s
	if strings.HasSuffix(s, ".*") {
		body = strings.TrimSuffix(s, ".*")
		if body == "" {
			return false
		}
	}
	for i := 0; i < len(body); i++ {
		ch := body[i]
		switch {
		case ch >= 'a' && ch <= 'z':
		case ch >= '0' && ch <= '9':
		case ch == '.', ch == '_', ch == '-':
		default:
			return false
		}
	}
	return true
}

// NormalizePatterns trims, validates, and deduplicates event patterns while
// preserving their order. It returns an empty slice when every entry is
// invalid; callers decide whether that is an error or an all-events default.
func NormalizePatterns(patterns []string) []string {
	out := make([]string, 0, len(patterns))
	seen := make(map[string]struct{}, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if !ValidType(pattern) {
			continue
		}
		if _, ok := seen[pattern]; ok {
			continue
		}
		seen[pattern] = struct{}{}
		out = append(out, pattern)
	}
	return out
}
