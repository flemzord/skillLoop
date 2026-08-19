package sanitize

import (
	"strings"
	"testing"
)

func TestTextRedactsSensitiveValues(t *testing.T) {
	input := "password=hunter2 email me@example.com /Users/alice/project https://example.test/a?signature=abc"
	result := Text(input)
	for _, forbidden := range []string{"hunter2", "me@example.com", "/Users/alice", "signature=abc"} {
		if strings.Contains(result, forbidden) {
			t.Fatalf("expected %q to be redacted from %q", forbidden, result)
		}
	}
}

func TestFingerprintRemovesVolatileNumbers(t *testing.T) {
	left := Fingerprint("go test failed with exit code 17")
	right := Fingerprint("go test failed with exit code 42")
	if left != right {
		t.Fatalf("expected stable fingerprints, got %q and %q", left, right)
	}
}
