package sanitize

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

const maximumFactLength = 320

var (
	privateKeyPattern = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`)
	secretPatterns    = []*regexp.Regexp{
		privateKeyPattern,
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{8,}\b`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
		regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{16,}\b`),
		regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{16,}\b`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
		regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
		regexp.MustCompile(`\b(?:AKIA|ASIA|AIDA|AROA|AIPA|ANPA|ANVA|APKA)[A-Z0-9]{16}\b`),
		regexp.MustCompile(`(?i)\baws[_ -]?secret[_ -]?access[_ -]?key\s*[:=]\s*["']?[A-Za-z0-9/+]{40}["']?`),
	}
	structuredSecretPatterns = []structuredSecretPattern{
		{
			pattern:     regexp.MustCompile(`(?i)["']?\b(?:password|passwd|secret|access[ _-]?token|api[ _-]?key|client[ _-]?secret|refresh[ _-]?token)["']?\s*[:=]\s*(?:"([^"\r\n]*)"|'([^'\r\n]*)'|([^\s,;}]+))`),
			valueGroups: []int{1, 2, 3},
		},
		{
			pattern:     regexp.MustCompile(`(?i)["']?\bauthorization["']?\s*[:=]\s*["']?bearer[ \t]+([-A-Za-z0-9._~+/=]+)["']?`),
			valueGroups: []int{1},
		},
	}
	emailPattern  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	queryPattern  = regexp.MustCompile(`https?://[^\s?]+\?[^\s]+`)
	homePattern   = regexp.MustCompile(`(?:/Users/|/home/)[^/\s]+`)
	spacesPattern = regexp.MustCompile(`\s+`)
	digitsPattern = regexp.MustCompile(`\b\d+\b`)
)

type structuredSecretPattern struct {
	pattern     *regexp.Regexp
	valueGroups []int
}

func Text(value string) string {
	value = RedactSecrets(value)
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

// ContainsSecret reports whether value contains a recognized credential. It is
// shared by durable learning sanitization and candidate validation so a token
// family cannot be redacted in one persistence path while reaching another.
func ContainsSecret(value string) bool {
	for _, pattern := range secretPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	for _, pattern := range structuredSecretPatterns {
		for _, match := range pattern.pattern.FindAllStringSubmatchIndex(value, -1) {
			if secretValue, ok := matchedSecretValue(value, match, pattern.valueGroups); ok && !isSecretPlaceholder(secretValue) {
				return true
			}
		}
	}
	return false
}

// RedactSecrets replaces recognized credentials without otherwise changing
// the input. Text applies the remaining privacy normalization afterwards.
func RedactSecrets(value string) string {
	for _, pattern := range secretPatterns {
		value = pattern.ReplaceAllString(value, "[REDACTED_SECRET]")
	}
	for _, pattern := range structuredSecretPatterns {
		value = redactStructuredSecrets(value, pattern)
	}
	return value
}

func redactStructuredSecrets(value string, pattern structuredSecretPattern) string {
	matches := pattern.pattern.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		return value
	}
	var redacted strings.Builder
	last := 0
	for _, match := range matches {
		secretValue, ok := matchedSecretValue(value, match, pattern.valueGroups)
		if !ok || isSecretPlaceholder(secretValue) {
			continue
		}
		redacted.WriteString(value[last:match[0]])
		redacted.WriteString("[REDACTED_SECRET]")
		last = match[1]
	}
	if last == 0 {
		return value
	}
	redacted.WriteString(value[last:])
	return redacted.String()
}

func matchedSecretValue(value string, match []int, groups []int) (string, bool) {
	for _, group := range groups {
		startIndex := group * 2
		if startIndex+1 >= len(match) || match[startIndex] < 0 {
			continue
		}
		return value[match[startIndex]:match[startIndex+1]], true
	}
	return "", false
}

func isSecretPlaceholder(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(value, "$") || strings.HasPrefix(value, "<") ||
		strings.HasPrefix(value, "{{") || strings.HasPrefix(value, "%") ||
		strings.HasPrefix(lower, "your_") || strings.HasPrefix(lower, "your-") ||
		strings.HasPrefix(lower, "example_") || strings.HasPrefix(lower, "example-") ||
		strings.HasPrefix(lower, "placeholder_") || strings.HasPrefix(lower, "placeholder-") {
		return true
	}
	return slices.Contains([]string{
		"example", "placeholder", "changeme", "change-me", "change_me", "fake", "test", "token", "xxx",
		"[redacted]", "[redacted_secret]",
	}, lower)
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
