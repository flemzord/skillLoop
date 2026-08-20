package cmd

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestTerminalSafeEscapesEveryTerminalControlClass(t *testing.T) {
	input := "safe\x00\x07\x1b]0;title\x07\x7f\r\n\t\u0080\u009f\u200e\u202e\u2066" + string([]byte{0xff, 0xfe})
	wantFragments := []string{
		"safe", `\x00`, `\x07`, `\x1b]0;title\x07`, `\x7f`, `\x0d`, `\x0a`, `\x09`,
		`\u0080`, `\u009f`, `\u200e`, `\u202e`, `\u2066`, `\xff`, `\xfe`,
	}

	output := terminalSafe(input)
	for _, fragment := range wantFragments {
		if !strings.Contains(output, fragment) {
			t.Fatalf("terminalSafe() = %q, want fragment %q", output, fragment)
		}
	}
	if !utf8.ValidString(output) {
		t.Fatalf("terminalSafe() returned invalid UTF-8: %q", output)
	}
	for _, character := range output {
		if character == 0x7f || unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
			t.Fatalf("terminalSafe() retained terminal control U+%04X in %q", character, output)
		}
	}
}
