package sanitize

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

const maximumFactLength = 320

var (
	secretPattern = regexp.MustCompile(`(?i)(?:sk-[a-z0-9_-]{12,}|gh[pousr]_[a-z0-9]{12,}|AKIA[A-Z0-9]{12,}|(?:token|secret|password|api[_-]?key)\s*[:=]\s*[^\s,;]+)`)
	emailPattern  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	queryPattern  = regexp.MustCompile(`https?://[^\s?]+\?[^\s]+`)
	homePattern   = regexp.MustCompile(`(?:/Users/|/home/)[^/\s]+`)
	spacesPattern = regexp.MustCompile(`\s+`)
	digitsPattern = regexp.MustCompile(`\b\d+\b`)
)

func Text(value string) string {
	value = secretPattern.ReplaceAllString(value, "[REDACTED_SECRET]")
	value = emailPattern.ReplaceAllString(value, "[REDACTED_EMAIL]")
	value = queryPattern.ReplaceAllStringFunc(value, func(raw string) string {
		if prefix, _, found := strings.Cut(raw, "?"); found {
			return prefix + "?[REDACTED_QUERY]"
		}
		return raw
	})
	value = homePattern.ReplaceAllString(value, "~")
	value = strings.TrimSpace(spacesPattern.ReplaceAllString(value, " "))
	runes := []rune(value)
	if len(runes) > maximumFactLength {
		value = string(runes[:maximumFactLength]) + "…"
	}
	return value
}

func Fingerprint(value string) string {
	value = strings.ToLower(Text(value))
	value = digitsPattern.ReplaceAllString(value, "#")
	value = strings.Map(func(character rune) rune {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || unicode.IsSpace(character) || character == '#' || character == '-' || character == '_' {
			return character
		}
		return ' '
	}, value)
	return strings.TrimSpace(spacesPattern.ReplaceAllString(value, " "))
}

func Path(value string) string {
	clean := filepath.Clean(value)
	return homePattern.ReplaceAllString(clean, "~")
}
