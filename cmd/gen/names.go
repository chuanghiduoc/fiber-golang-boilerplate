package main

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

var (
	// snakeName is a lower_snake_case identifier. It also rejects path
	// separators, dots, whitespace, leading digits, and traversal sequences.
	snakeName = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	// pascalName is a use-case name such as IssueInvoice.
	pascalName = regexp.MustCompile(`^[A-Z][a-zA-Z0-9]*$`)
)

// goKeywords cannot be package or file-derived identifiers.
var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
}

// reservedModuleNames collide with the repository layout, a top-level
// package, or a Go-reserved directory the go tool ignores.
var reservedModuleNames = map[string]bool{
	"app": true, "main": true, "internal": true, "modules": true, "platform": true,
	"pkg": true, "handler": true, "service": true, "repository": true, "dto": true,
	"sqlc": true, "config": true, "testutil": true, "middleware": true, "authz": true,
	"audit": true, "jobs": true, "session": true, "bootstrap": true, "contracts": true,
	"catalog": true, "queries": true, "domain": true, "worker": true, "adapter": true,
	"vendor": true, "testdata": true,
}

// reservedResourceNames would produce file or type names that clash with the
// module skeleton (service/health.go, service/ports.go, handler/health.go).
var reservedResourceNames = map[string]bool{
	"health": true, "port": true, "ports": true, "authorize": true, "main": true,
}

func validateModuleName(name string) error {
	if !snakeName.MatchString(name) {
		return fmt.Errorf("module name %q must be lower_snake_case (for example billing)", name)
	}
	if goKeywords[name] {
		return fmt.Errorf("module name %q is a Go keyword", name)
	}
	if reservedModuleNames[name] {
		return fmt.Errorf("module name %q is reserved by the repository layout", name)
	}
	return nil
}

func validateResourceName(name string) error {
	if !snakeName.MatchString(name) {
		return fmt.Errorf("resource name %q must be singular lower_snake_case (for example invoice)", name)
	}
	if goKeywords[name] || reservedResourceNames[name] {
		return fmt.Errorf("resource name %q is reserved", name)
	}
	return nil
}

func validateUseCaseName(name string) error {
	if !pascalName.MatchString(name) {
		return fmt.Errorf("use-case name %q must be PascalCase (for example IssueInvoice)", name)
	}
	if len(snakeWords(name)) < 2 {
		return fmt.Errorf("use-case name %q must have a verb and a noun (for example IssueInvoice)", name)
	}
	return nil
}

// pascal turns lower_snake_case into PascalCase (line_item -> LineItem).
func pascal(snake string) string {
	var b strings.Builder
	for _, word := range strings.Split(snake, "_") {
		if word == "" {
			continue
		}
		b.WriteString(strings.ToUpper(word[:1]) + word[1:])
	}
	return b.String()
}

// camel turns lower_snake_case into camelCase (line_item -> lineItem).
func camel(snake string) string {
	p := pascal(snake)
	if p == "" {
		return p
	}
	return strings.ToLower(p[:1]) + p[1:]
}

// snakeWords splits PascalCase into lower-case words (IssueInvoice ->
// [issue invoice]). A run of capitals stays one word (ExportCSV -> [export csv]).
func snakeWords(name string) []string {
	runes := []rune(name)
	var words []string
	start := 0
	for i := 1; i < len(runes); i++ {
		lowerToUpper := unicode.IsUpper(runes[i]) && unicode.IsLower(runes[i-1])
		acronymEnd := unicode.IsUpper(runes[i]) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) && unicode.IsUpper(runes[i-1])
		if lowerToUpper || acronymEnd {
			words = append(words, strings.ToLower(string(runes[start:i])))
			start = i
		}
	}
	return append(words, strings.ToLower(string(runes[start:])))
}

// snakeFromPascal turns PascalCase into lower_snake_case.
func snakeFromPascal(name string) string { return strings.Join(snakeWords(name), "_") }

// plural is a deliberately small English pluralizer for table and route
// names. Irregular nouns take --plural.
func plural(word string) string {
	switch {
	case strings.HasSuffix(word, "y") && len(word) > 1 && !strings.ContainsRune("aeiou", rune(word[len(word)-2])):
		return word[:len(word)-1] + "ies"
	case strings.HasSuffix(word, "s"), strings.HasSuffix(word, "x"), strings.HasSuffix(word, "z"),
		strings.HasSuffix(word, "ch"), strings.HasSuffix(word, "sh"):
		return word + "es"
	default:
		return word + "s"
	}
}

// pluralSnake pluralizes the last word of a snake_case name (line_item ->
// line_items).
func pluralSnake(snake string) string {
	i := strings.LastIndex(snake, "_")
	return snake[:i+1] + plural(snake[i+1:])
}
