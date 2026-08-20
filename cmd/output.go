package cmd

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// terminalSafe renders lower-trust values as a single inert terminal field.
// Machine-readable JSON must continue to use the original values directly.
func terminalSafe(value string) string {
	var output strings.Builder
	for len(value) > 0 {
		if value[0] < utf8.RuneSelf {
			character := value[0]
			value = value[1:]
			if character >= 0x20 && character <= 0x7e {
				output.WriteByte(character)
			} else {
				_, _ = fmt.Fprintf(&output, `\x%02x`, character)
			}
			continue
		}

		character, size := utf8.DecodeRuneInString(value)
		if character == utf8.RuneError && size == 1 {
			_, _ = fmt.Fprintf(&output, `\x%02x`, value[0])
			value = value[1:]
			continue
		}
		value = value[size:]
		if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
			_, _ = fmt.Fprintf(&output, `\u%04x`, character)
			continue
		}
		output.WriteRune(character)
	}
	return output.String()
}
