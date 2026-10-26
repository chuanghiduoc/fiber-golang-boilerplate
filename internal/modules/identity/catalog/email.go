package catalog

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"
)

// Email templates live in the same en/vi catalogs as the API strings, so the
// startup parity check (i18n.New) covers their keys and {placeholders}. This
// loader exists because the identity service is framework-free and cannot
// import internal/platform/i18n (which links Fiber).
var (
	emailTextOnce     sync.Once
	emailTextCatalogs map[string]map[string]string
)

// EmailText returns the localized email string for key and reports whether the
// key exists. Placeholders use the same {name} syntax as the API catalogs.
// The caller must treat a missing key as a build defect: i18n.New rejects a
// catalog whose locales disagree, but it cannot know which keys a service
// reads.
func EmailText(locale, key string, params map[string]any) (string, bool) {
	emailTextOnce.Do(loadEmailText)
	if key == "" {
		return "", false
	}
	if message, ok := emailTextCatalogs[normalizeEmailLocale(locale)][key]; ok {
		return interpolateEmail(message, params), true
	}
	if message, ok := emailTextCatalogs["en"][key]; ok {
		return interpolateEmail(message, params), true
	}
	return "", false
}

func loadEmailText() {
	files, err := fs.Glob(localesFS, "locales/*.json")
	if err != nil {
		panic(fmt.Sprintf("identity catalog: list email locales: %v", err))
	}
	emailTextCatalogs = make(map[string]map[string]string, len(files))
	for _, file := range files {
		raw, err := fs.ReadFile(localesFS, file)
		if err != nil {
			panic(fmt.Sprintf("identity catalog: read %q: %v", file, err))
		}
		var messages map[string]string
		if err := json.Unmarshal(raw, &messages); err != nil {
			panic(fmt.Sprintf("identity catalog: decode %q: %v", file, err))
		}
		locale := strings.ToLower(strings.TrimSuffix(path.Base(file), path.Ext(file)))
		emailTextCatalogs[locale] = messages
	}
}

// normalizeEmailLocale matches a BCP 47 tag against the loaded catalogs,
// truncating a regional tag (vi-VN -> vi) and falling back to English.
func normalizeEmailLocale(locale string) string {
	locale = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(locale), "_", "-"))
	if _, ok := emailTextCatalogs[locale]; ok {
		return locale
	}
	if tag, _, found := strings.Cut(locale, "-"); found && tag != "" {
		if _, ok := emailTextCatalogs[tag]; ok {
			return tag
		}
	}
	return "en"
}

// interpolateEmail replaces {name} placeholders with params, leaving unknown
// placeholders intact (the same rule as internal/platform/i18n).
func interpolateEmail(message string, params map[string]any) string {
	if len(params) == 0 || !strings.Contains(message, "{") {
		return message
	}
	var out strings.Builder
	out.Grow(len(message))
	for i := 0; i < len(message); {
		start := strings.IndexByte(message[i:], '{')
		if start < 0 {
			out.WriteString(message[i:])
			break
		}
		start += i
		out.WriteString(message[i:start])
		end := strings.IndexByte(message[start:], '}')
		if end < 0 {
			out.WriteString(message[start:])
			break
		}
		end += start
		key := strings.TrimSpace(message[start+1 : end])
		if value, ok := params[key]; ok {
			fmt.Fprint(&out, value)
		} else {
			out.WriteString(message[start : end+1])
		}
		i = end + 1
	}
	return out.String()
}
