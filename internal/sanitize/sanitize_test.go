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

func TestSecretClassifierAndRedactorCoverDurableCredentialFamilies(t *testing.T) {
	tests := map[string]string{
		"jwt":                 "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlX3ZhbHVl",
		"github fine grained": "github_pat_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUV",
		"gitlab":              "glpat-abcdefghijklmnopqrstuvwx",
		"slack":               "xoxb-" + "123456789012-123456789012-abcdefghijklmnopqrstuvwx",
		"private key":         "-----BEGIN PRIVATE KEY-----\nZmFrZS1wcml2YXRlLWtleQ==\n-----END PRIVATE KEY-----",
		"aws access key":      "AKIA" + "ABCDEFGHIJKLMNOP",
		"aws secret":          "aws_secret_access_key=" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN",
		"client secret":       `client_secret="a-real-client-secret-value"`,
		"refresh token":       `"refresh_token": "refresh-value-abcdefghijklmnopqrstuvwxyz"`,
		"opaque bearer":       `Authorization: Bearer mF_9.B5f-4.1JqM/opaque-token-value`,
		"json quoted key":     `{"api_key":"json-secret-value-12345"}`,
	}

	for name, credential := range tests {
		t.Run(name, func(t *testing.T) {
			input := "before " + credential + " after"
			if !ContainsSecret(input) {
				t.Fatalf("credential family was not classified: %q", credential)
			}
			redacted := RedactSecrets(input)
			if !strings.Contains(redacted, "[REDACTED_SECRET]") || ContainsSecret(redacted) {
				t.Fatalf("credential was not completely redacted: %q", redacted)
			}
			if strings.Contains(Text(input), credential) {
				t.Fatalf("credential survived durable text sanitization: %q", Text(input))
			}
		})
	}
}

func TestSecretClassifierPreservesSafeLookalikes(t *testing.T) {
	for _, value := range []string{
		"github_pat_example",
		"glpat-placeholder",
		"eyJ.not.a-jwt",
		"xoxb-example",
		"AKIAEXAMPLE",
		"aws_secret_access_key=${AWS_SECRET_ACCESS_KEY}",
		"password=${PASSWORD}",
		"client_secret=${CLIENT_SECRET}",
		`"refresh_token": "${REFRESH_TOKEN}"`,
		"client_secret=placeholder",
		`"refresh_token": "example"`,
		"Authorization: Bearer ${ACCESS_TOKEN}",
		"Authorization: Bearer <token>",
		"Authorization: Bearer example",
		"client_secret_hint=public-identifier",
		"-----BEGIN PUBLIC KEY-----",
	} {
		if ContainsSecret(value) {
			t.Errorf("safe lookalike classified as a credential: %q", value)
		}
		if got := RedactSecrets(value); got != value {
			t.Errorf("safe lookalike changed: got %q want %q", got, value)
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

func TestTextTruncationPreservesUTF8(t *testing.T) {
	result := Text(strings.Repeat("é", maximumFactLength+1))
	if !strings.HasSuffix(result, "…") {
		t.Fatalf("expected truncated value, got %q", result)
	}
	if got := len([]rune(strings.TrimSuffix(result, "…"))); got != maximumFactLength {
		t.Fatalf("expected %d runes, got %d", maximumFactLength, got)
	}
}
