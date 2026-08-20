package sanitize

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const maximumFactLength = 320

var (
	privateKeyPattern = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|\z)`)
	secretPatterns    = []*regexp.Regexp{
		privateKeyPattern,
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{8,}\b`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
		regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{16,}\b`),
		regexp.MustCompile(`\bhf_[A-Za-z0-9]{20,}\b`),
		regexp.MustCompile(`\bnpm_[A-Za-z0-9]{20,}\b`),
		regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{16,}\b`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
		regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
		regexp.MustCompile(`\b(?:sk|rk)_live_[A-Za-z0-9]{16,}\b`),
		regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{35}`),
		regexp.MustCompile(`\b(?:AKIA|ASIA|AIDA|AROA|AIPA|ANPA|ANVA|APKA)[A-Z0-9]{16}\b`),
		regexp.MustCompile(`(?i)\baws[_ -]?secret[_ -]?access[_ -]?key\s*[:=]\s*["']?[A-Za-z0-9/+]{40}["']?`),
	}
	structuredSecretPatterns = []structuredSecretPattern{
		{
			pattern: regexp.MustCompile(
				`(?i)["']?\b([A-Za-z_][A-Za-z0-9_-]*(?:[ \t]+(?:token|key|secret))?)["']?\s*[:=]\s*` +
					`(?:"((?:\\.|[^"\\\r\n])*)"|'((?:\\.|[^'\\\r\n])*)'|("[^\r\n]*)|('[^\r\n]*)|([^\s,;]*))`,
			),
			keyGroup:    1,
			valueGroups: []int{2, 3, 4, 5, 6},
		},
		{
			pattern: regexp.MustCompile(
				`(?i)["']?\bauthorization["']?\s*[:=]\s*` +
					`(?:"(?:bearer|basic)[ \t]+((?:\\.|[^"\\\r\n])*)"|'(?:bearer|basic)[ \t]+((?:\\.|[^'\\\r\n])*)'|` +
					`("(?:bearer|basic)[ \t]+[^\r\n]*)|('(?:bearer|basic)[ \t]+[^\r\n]*)|(?:bearer|basic)[ \t]+([^\s,;]*))`,
			),
			valueGroups: []int{1, 2, 3, 4, 5},
		},
	}
	credentialURIPattern  = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^/?#@\s]*:([^/?#@\s]+)@`)
	mysqlDSNPattern       = regexp.MustCompile(`(?i)(?:^|[\s,{])["']?([A-Za-z_][A-Za-z0-9_-]*)["']?\s*[:=]\s*["']?[^:@\s;,]*:([^@\s;,]+)@(?:tcp|unix)\(`)
	odbcContextPattern    = regexp.MustCompile(`(?i)(?:^|[;,{ \t])["']?(?:odbc_dsn|connection_string|database_dsn|database_url|dsn|driver|server|data[ _-]?source|uid|user[ _-]?id)["']?[ \t]*[:=]`)
	odbcWrapperPattern    = regexp.MustCompile(`(?i)(?:^|[\s,{])["']?(?:odbc_dsn|connection_string|database_dsn|database_url|dsn)["']?\s*[:=]`)
	odbcHintPattern       = regexp.MustCompile(`(?i)(?:^|[\s,{])["']?(?:odbc_dsn|connection_string|database_dsn|database_url|dsn)(?:_hint|_example)["']?\s*[:=]`)
	odbcPasswordPattern   = regexp.MustCompile(`(?i)(?:^|;)[ \t]*pwd[ \t]*=[ \t]*([^;\r\n]*)`)
	assignmentLinePattern = regexp.MustCompile(
		`(?i)^([ \t]*)(?:-[ \t]+)?(?:export[ \t]+)?["']?([A-Za-z_][A-Za-z0-9_-]*(?:[ \t]+(?:token|key|secret))?)["']?[ \t]*[:=][ \t]*(.*)$`,
	)
	fieldLinePattern       = regexp.MustCompile(`(?i)^[ \t]*(?:-[ \t]+)?(?:export[ \t]+)?(?:["'][^"'\r\n]+["']|[A-Za-z_][A-Za-z0-9_.-]*)[ \t]*[:=]`)
	yamlBlockScalarPattern = regexp.MustCompile(`^(?:(?:![^\s]+|&[A-Za-z0-9_-]+)[ \t]+)*[|>](?:[+-](?:[1-9])?|[1-9][+-]?)?[ \t]*(?:#.*)?$`)
	emailPattern           = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	queryPattern           = regexp.MustCompile(`https?://[^\s?]+\?[^\s]+`)
	homePattern            = regexp.MustCompile(`(?:/Users/|/home/)[^/\s]+`)
	spacesPattern          = regexp.MustCompile(`\s+`)
	digitsPattern          = regexp.MustCompile(`\b\d+\b`)
	placeholderPatterns    = []*regexp.Regexp{
		regexp.MustCompile(`^\$(?:[A-Za-z_][A-Za-z0-9_]*|\{[A-Za-z_][A-Za-z0-9_]*\})$`),
		regexp.MustCompile(`^<[A-Za-z_][A-Za-z0-9_.-]*>$`),
		regexp.MustCompile(`^\{\{\s*[A-Za-z_][A-Za-z0-9_.-]*\s*\}\}$`),
		regexp.MustCompile(`^%[A-Za-z_][A-Za-z0-9_]*%$`),
		regexp.MustCompile(`(?i)^(?:your|example|placeholder)[_-](?:(?:api|access|refresh|client|auth|private|secret)[_-])?(?:password|passwd|secret|token|key|value|credential)$`),
		regexp.MustCompile(`(?i)^(?:hf|npm)_(?:example|placeholder|test|fake|x+)$`),
		regexp.MustCompile(`(?i)^(?:sk|rk)_live_(?:example|placeholder|test|fake|x+)$`),
	}
	placeholderWords = map[string]struct{}{
		"example": {}, "placeholder": {}, "changeme": {}, "change-me": {}, "change_me": {},
		"fake": {}, "test": {}, "token": {}, "xxx": {}, "[redacted]": {}, "[redacted_secret]": {},
	}
)

type structuredSecretPattern struct {
	pattern     *regexp.Regexp
	keyGroup    int
	valueGroups []int
}

type secretRange struct {
	start int
	end   int
}

type multilineValueRange struct {
	secretRange
	placeholder bool
}

type rawPlaceholderAnchors struct {
	exact     map[secretRange]struct{}
	multiline []secretRange
}

type textLine struct {
	start      int
	contentEnd int
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
	multilineValues := multilineValueRanges(value)
	var anchors rawPlaceholderAnchors
	anchorsReady := false
	for _, pattern := range secretPatterns {
		for _, match := range pattern.FindAllStringIndex(value, -1) {
			if !anchorsReady {
				anchors = buildRawPlaceholderAnchors(value, multilineValues)
				anchorsReady = true
			}
			if !isAnchoredRawPlaceholder(value, match[0], match[1], anchors) {
				return true
			}
		}
	}
	if len(credentialSecretRanges(value)) > 0 || len(unsafeMultilineRanges(multilineValues)) > 0 {
		return true
	}
	for _, pattern := range structuredSecretPatterns {
		for _, match := range pattern.pattern.FindAllStringSubmatchIndex(value, -1) {
			if !matchesSensitiveKey(value, match, pattern) {
				continue
			}
			if secretValue, ok := matchedSecretValue(value, match, pattern.valueGroups); ok &&
				!isSafeDeferredMultilineMatch(secretValue, match, multilineValues) && !isSecretPlaceholder(secretValue) {
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
		matches := pattern.FindAllStringIndex(value, -1)
		if len(matches) == 0 {
			continue
		}
		anchors := buildRawPlaceholderAnchors(value, multilineValueRanges(value))
		value = redactRawSecrets(value, matches, anchors)
	}
	value = redactSecretRanges(value, credentialSecretRanges(value))
	value = redactSecretRanges(value, unsafeMultilineRanges(multilineValueRanges(value)))
	multilineValues := multilineValueRanges(value)
	for _, pattern := range structuredSecretPatterns {
		value = redactStructuredSecrets(value, pattern, multilineValues)
	}
	return value
}

func redactRawSecrets(value string, matches [][]int, anchors rawPlaceholderAnchors) string {
	var redacted strings.Builder
	last := 0
	for _, match := range matches {
		if isAnchoredRawPlaceholder(value, match[0], match[1], anchors) {
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

func redactStructuredSecrets(value string, pattern structuredSecretPattern, multilineValues []multilineValueRange) string {
	matches := pattern.pattern.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		return value
	}
	var redacted strings.Builder
	last := 0
	for _, match := range matches {
		if !matchesSensitiveKey(value, match, pattern) {
			continue
		}
		secretValue, ok := matchedSecretValue(value, match, pattern.valueGroups)
		if !ok || isSafeDeferredMultilineMatch(secretValue, match, multilineValues) || isSecretPlaceholder(secretValue) {
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

// credentialSecretRanges recognizes credential-bearing connection strings
// while leaving host-only, username-only, hint, and placeholder forms alone.
func credentialSecretRanges(value string) []secretRange {
	ranges := credentialURISecretRanges(value)
	ranges = append(ranges, mysqlDSNSecretRanges(value)...)
	ranges = append(ranges, odbcSecretRanges(value)...)
	sort.Slice(ranges, func(left, right int) bool { return ranges[left].start < ranges[right].start })
	return ranges
}

func credentialURISecretRanges(value string) []secretRange {
	matches := credentialURIPattern.FindAllStringSubmatchIndex(value, -1)
	ranges := make([]secretRange, 0, len(matches))
	for _, match := range matches {
		if len(match) < 4 || match[2] < 0 || isSecretPlaceholder(value[match[2]:match[3]]) {
			continue
		}
		ranges = append(ranges, secretRange{start: match[2], end: match[3]})
	}
	return ranges
}

func mysqlDSNSecretRanges(value string) []secretRange {
	matches := mysqlDSNPattern.FindAllStringSubmatchIndex(value, -1)
	ranges := make([]secretRange, 0, len(matches))
	for _, match := range matches {
		if len(match) < 6 || match[2] < 0 || !isDSNContextKey(value[match[2]:match[3]]) || match[4] < 0 ||
			isSecretPlaceholder(value[match[4]:match[5]]) {
			continue
		}
		ranges = append(ranges, secretRange{start: match[4], end: match[5]})
	}
	return ranges
}

func odbcSecretRanges(value string) []secretRange {
	lines := splitTextLines(value)
	var ranges []secretRange
	for _, line := range lines {
		content := value[line.start:line.contentEnd]
		if !odbcContextPattern.MatchString(content) {
			continue
		}
		hintMatches := odbcHintPattern.FindAllStringIndex(content, -1)
		wrapperMatches := odbcWrapperPattern.FindAllStringIndex(content, -1)
		for _, match := range odbcPasswordPattern.FindAllStringSubmatchIndex(content, -1) {
			if len(match) < 4 || match[2] < 0 ||
				lastMatchStartBefore(hintMatches, match[0]) > lastMatchStartBefore(wrapperMatches, match[0]) {
				continue
			}
			secret, placeholder := odbcPasswordRange(content, match[2], match[3])
			if placeholder {
				continue
			}
			ranges = append(ranges, secretRange{start: line.start + secret.start, end: line.start + secret.end})
		}
	}
	return ranges
}

func odbcPasswordRange(value string, start, end int) (secretRange, bool) {
	for start < end && (value[start] == ' ' || value[start] == '\t') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\t') {
		end--
	}
	innerStart, innerEnd := start, end
	if end-start >= 2 {
		switch {
		case value[start] == '{' && value[end-1] == '}':
			innerStart, innerEnd = start+1, end-1
		case (value[start] == '"' || value[start] == '\'') && value[end-1] == value[start]:
			innerStart, innerEnd = start+1, end-1
		}
	}
	if isSecretPlaceholder(value[innerStart:innerEnd]) {
		return secretRange{}, true
	}
	return secretRange{start: innerStart, end: innerEnd}, false
}

func lastMatchStartBefore(matches [][]int, limit int) int {
	index := sort.Search(len(matches), func(index int) bool { return matches[index][0] >= limit }) - 1
	if index < 0 {
		return -1
	}
	return matches[index][0]
}

func isDSNContextKey(value string) bool {
	switch normalizeKey(value) {
	case "database_url", "mysql_url", "database_dsn", "mysql_dsn", "connection_string", "dsn":
		return true
	default:
		return false
	}
}

// multilineValueRanges owns encodings whose value is not confined to the
// assignment line. Handling them before the single-line patterns prevents a
// harmless-looking header from being redacted while its credential body is
// left available to durable learning records.
func multilineValueRanges(value string) []multilineValueRange {
	lines := splitTextLines(value)
	var ranges []multilineValueRange
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		headerStart, headerIndent, rawValue, ok := findStructuredAssignment(value[line.start:line.contentEnd])
		if !ok {
			continue
		}
		headerStart += line.start

		switch {
		case isYAMLBlockScalarIndicator(rawValue):
			endIndex, semanticValue := yamlBlockValue(value, lines, index, headerIndent)
			ranges = append(ranges, multilineValueRange{
				secretRange: secretRange{start: headerStart, end: lines[endIndex].contentEnd},
				placeholder: isSecretPlaceholder(semanticValue),
			})
			index = endIndex
		case hasLineContinuation(rawValue):
			endIndex, semanticValue := continuedValue(value, lines, index, rawValue)
			ranges = append(ranges, multilineValueRange{
				secretRange: secretRange{start: headerStart, end: lines[endIndex].contentEnd},
				placeholder: isSecretPlaceholder(semanticValue),
			})
			index = endIndex
		case startsUnclosedQuote(rawValue):
			endIndex, end := multilineQuotedValueEnd(value, lines, index, rawValue[0])
			ranges = append(ranges, multilineValueRange{secretRange: secretRange{start: headerStart, end: end}})
			index = endIndex
		default:
			endIndex, semanticValue, continued := yamlPlainContinuationValue(value, lines, index, headerIndent, rawValue)
			if continued {
				ranges = append(ranges, multilineValueRange{
					secretRange: secretRange{start: headerStart, end: lines[endIndex].contentEnd},
					placeholder: isSecretPlaceholder(semanticValue),
				})
				index = endIndex
			}
		}
	}
	return ranges
}

func findStructuredAssignment(line string) (start, indent int, rawValue string, found bool) {
	if match := assignmentLinePattern.FindStringSubmatchIndex(line); match != nil &&
		isSensitiveKey(submatch(line, match, 2)) {
		rawValue = submatch(line, match, 3)
		return 0, len(submatch(line, match, 1)), rawValue, true
	}

	for _, pattern := range structuredSecretPatterns {
		for _, match := range pattern.pattern.FindAllStringSubmatchIndex(line, -1) {
			if !matchesSensitiveKey(line, match, pattern) {
				continue
			}
			rawValue, ok := matchedSecretValue(line, match, pattern.valueGroups)
			if ok {
				return match[0], leadingWhitespace(line), rawValue, true
			}
		}
	}
	return 0, 0, "", false
}

func unsafeMultilineRanges(values []multilineValueRange) []secretRange {
	ranges := make([]secretRange, 0, len(values))
	for _, value := range values {
		if !value.placeholder {
			ranges = append(ranges, value.secretRange)
		}
	}
	return ranges
}

func splitTextLines(value string) []textLine {
	lines := make([]textLine, 0, strings.Count(value, "\n")+1)
	for start := 0; start < len(value); {
		relativeEnd := strings.IndexByte(value[start:], '\n')
		rawEnd := len(value)
		end := len(value)
		if relativeEnd >= 0 {
			rawEnd = start + relativeEnd
			end = rawEnd + 1
		}
		contentEnd := rawEnd
		if contentEnd > start && value[contentEnd-1] == '\r' {
			contentEnd--
		}
		lines = append(lines, textLine{start: start, contentEnd: contentEnd})
		start = end
	}
	if len(lines) == 0 {
		return []textLine{{}}
	}
	return lines
}

func yamlBlockValue(value string, lines []textLine, headerIndex, headerIndent int) (int, string) {
	endIndex := headerIndex
	var body strings.Builder
	indentedBody := false
	for index := headerIndex + 1; index < len(lines); index++ {
		content := value[lines[index].start:lines[index].contentEnd]
		if strings.TrimSpace(content) != "" {
			indent := leadingWhitespace(content)
			if indent <= headerIndent {
				// A mapping/assignment is a reliable sibling boundary. A bare,
				// non-indented line immediately after the header is malformed
				// YAML, so consume it fail-closed instead of leaking it.
				if fieldLinePattern.MatchString(content) || indentedBody {
					break
				}
			} else {
				indentedBody = true
			}
		}
		if body.Len() > 0 {
			body.WriteByte('\n')
		}
		body.WriteString(strings.TrimSpace(content))
		endIndex = index
	}
	return endIndex, strings.TrimSpace(body.String())
}

func yamlPlainContinuationValue(
	value string,
	lines []textLine,
	headerIndex, headerIndent int,
	rawValue string,
) (int, string, bool) {
	endIndex := headerIndex
	continued := false
	var semantic strings.Builder
	semantic.WriteString(strings.TrimSpace(rawValue))
	for index := headerIndex + 1; index < len(lines); index++ {
		content := value[lines[index].start:lines[index].contentEnd]
		if strings.TrimSpace(content) == "" {
			continue
		}
		if leadingWhitespace(content) <= headerIndent {
			break
		}
		continued = true
		endIndex = index
		semantic.WriteByte(' ')
		semantic.WriteString(strings.TrimSpace(content))
	}
	return endIndex, strings.TrimSpace(semantic.String()), continued
}

func continuedValue(value string, lines []textLine, headerIndex int, rawValue string) (int, string) {
	endIndex := headerIndex
	var semantic strings.Builder
	part := strings.TrimSpace(rawValue)
	for {
		continued := hasLineContinuation(part)
		if continued {
			part = strings.TrimSpace(part[:len(part)-1])
		}
		semantic.WriteString(part)
		if !continued || endIndex+1 >= len(lines) {
			break
		}
		endIndex++
		part = strings.TrimSpace(value[lines[endIndex].start:lines[endIndex].contentEnd])
	}
	return endIndex, strings.TrimSpace(semantic.String())
}

func multilineQuotedValueEnd(value string, lines []textLine, headerIndex int, quote byte) (int, int) {
	for index := headerIndex + 1; index < len(lines); index++ {
		content := value[lines[index].start:lines[index].contentEnd]
		if closing := unescapedQuoteIndex(content, quote, 0); closing >= 0 {
			return index, lines[index].start + closing + 1
		}
	}
	last := len(lines) - 1
	return last, lines[last].contentEnd
}

func redactSecretRanges(value string, ranges []secretRange) string {
	if len(ranges) == 0 {
		return value
	}
	var redacted strings.Builder
	last := 0
	for _, secret := range ranges {
		if secret.start < last || secret.start < 0 || secret.end < secret.start || secret.end > len(value) {
			continue
		}
		redacted.WriteString(value[last:secret.start])
		redacted.WriteString("[REDACTED_SECRET]")
		last = secret.end
	}
	if last == 0 {
		return value
	}
	redacted.WriteString(value[last:])
	return redacted.String()
}

func submatch(value string, match []int, group int) string {
	start := group * 2
	if start+1 >= len(match) || match[start] < 0 {
		return ""
	}
	return value[match[start]:match[start+1]]
}

func leadingWhitespace(value string) int {
	return len(value) - len(strings.TrimLeft(value, " \t"))
}

func isYAMLBlockScalarIndicator(value string) bool {
	return yamlBlockScalarPattern.MatchString(strings.TrimSpace(value))
}

func hasLineContinuation(value string) bool {
	value = strings.TrimRight(value, " \t")
	backslashes := 0
	for index := len(value) - 1; index >= 0 && value[index] == '\\'; index-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func startsUnclosedQuote(value string) bool {
	value = strings.TrimSpace(value)
	return len(value) > 0 && (value[0] == '\'' || value[0] == '"') && unescapedQuoteIndex(value, value[0], 1) < 0
}

func unescapedQuoteIndex(value string, quote byte, start int) int {
	escaped := false
	for index := start; index < len(value); index++ {
		switch {
		case escaped:
			escaped = false
		case value[index] == '\\':
			escaped = true
		case value[index] == quote:
			return index
		}
	}
	return -1
}

func isSafeDeferredMultilineMatch(value string, match []int, multilineValues []multilineValueRange) bool {
	if !isYAMLBlockScalarIndicator(value) && !hasLineContinuation(value) {
		return false
	}
	index := sort.Search(len(multilineValues), func(index int) bool {
		return multilineValues[index].start > match[0]
	}) - 1
	if index >= 0 && multilineValues[index].placeholder && match[1] <= multilineValues[index].end {
		return true
	}
	return false
}

func buildRawPlaceholderAnchors(value string, multilineValues []multilineValueRange) rawPlaceholderAnchors {
	anchors := rawPlaceholderAnchors{exact: make(map[secretRange]struct{})}
	trimmedStart := len(value) - len(strings.TrimLeftFunc(value, unicode.IsSpace))
	trimmedEnd := len(strings.TrimRightFunc(value, unicode.IsSpace))
	if trimmedStart < trimmedEnd {
		anchors.exact[secretRange{start: trimmedStart, end: trimmedEnd}] = struct{}{}
	}
	for _, multiline := range multilineValues {
		if multiline.placeholder {
			anchors.multiline = append(anchors.multiline, multiline.secretRange)
		}
	}
	for _, pattern := range structuredSecretPatterns {
		for _, match := range pattern.pattern.FindAllStringSubmatchIndex(value, -1) {
			if !matchesSensitiveKey(value, match, pattern) {
				continue
			}
			start, end, ok := matchedSecretValueRange(match, pattern.valueGroups)
			if ok && isSecretPlaceholder(value[start:end]) {
				anchors.exact[secretRange{start: start, end: end}] = struct{}{}
			}
		}
	}
	return anchors
}

func isAnchoredRawPlaceholder(value string, start, end int, anchors rawPlaceholderAnchors) bool {
	candidate := value[start:end]
	if !isSecretPlaceholder(candidate) {
		return false
	}
	if _, ok := anchors.exact[secretRange{start: start, end: end}]; ok {
		return true
	}
	index := sort.Search(len(anchors.multiline), func(index int) bool {
		return anchors.multiline[index].start > start
	}) - 1
	if index >= 0 && end <= anchors.multiline[index].end {
		return true
	}
	return false
}

func matchesSensitiveKey(value string, match []int, pattern structuredSecretPattern) bool {
	if pattern.keyGroup == 0 {
		return true
	}
	start := pattern.keyGroup * 2
	if start+1 >= len(match) || match[start] < 0 {
		return false
	}
	return isSensitiveKey(value[match[start]:match[start+1]])
}

func isSensitiveKey(value string) bool {
	normalized := normalizeKey(value)
	switch normalized {
	case "password", "passwd", "secret", "token", "access_token", "api_key", "client_secret", "refresh_token", "secret_key", "private_key":
		return true
	default:
		return strings.HasSuffix(normalized, "_token") ||
			strings.HasSuffix(normalized, "_api_key") ||
			strings.HasSuffix(normalized, "_secret_key") ||
			strings.HasSuffix(normalized, "_private_key") ||
			strings.HasSuffix(normalized, "_password") ||
			strings.HasSuffix(normalized, "_passwd") ||
			strings.HasSuffix(normalized, "_secret")
	}
}

func normalizeKey(value string) string {
	runes := []rune(strings.TrimSpace(value))
	var normalized strings.Builder
	separator := true
	for index, character := range runes {
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) {
			if normalized.Len() > 0 {
				separator = true
			}
			continue
		}
		if unicode.IsUpper(character) && !separator && index > 0 {
			previous := runes[index-1]
			nextIsLower := index+1 < len(runes) && unicode.IsLower(runes[index+1])
			if unicode.IsLower(previous) || unicode.IsDigit(previous) || (unicode.IsUpper(previous) && nextIsLower) {
				normalized.WriteByte('_')
			}
		}
		if separator && normalized.Len() > 0 {
			normalized.WriteByte('_')
		}
		normalized.WriteRune(unicode.ToLower(character))
		separator = false
	}
	return strings.Trim(normalized.String(), "_")
}

func matchedSecretValue(value string, match []int, groups []int) (string, bool) {
	start, end, ok := matchedSecretValueRange(match, groups)
	if !ok {
		return "", false
	}
	return value[start:end], true
}

func matchedSecretValueRange(match []int, groups []int) (int, int, bool) {
	for _, group := range groups {
		startIndex := group * 2
		if startIndex+1 >= len(match) || match[startIndex] < 0 {
			continue
		}
		return match[startIndex], match[startIndex+1], true
	}
	return 0, 0, false
}

func isSecretPlaceholder(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, pattern := range placeholderPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	_, ok := placeholderWords[strings.ToLower(value)]
	return ok
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
