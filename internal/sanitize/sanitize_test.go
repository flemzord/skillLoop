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
		"jwt":                   "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlX3ZhbHVl",
		"github fine grained":   "github_pat_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUV",
		"gitlab":                "glpat-abcdefghijklmnopqrstuvwx",
		"slack":                 "xoxb-" + "123456789012-123456789012-abcdefghijklmnopqrstuvwx",
		"private key":           "-----BEGIN PRIVATE KEY-----\nZmFrZS1wcml2YXRlLWtleQ==\n-----END PRIVATE KEY-----",
		"aws access key":        "AKIA" + "ABCDEFGHIJKLMNOP",
		"aws secret":            "aws_secret_access_key=" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN",
		"client secret":         `client_secret="a-real-client-secret-value"`,
		"refresh token":         `"refresh_token": "refresh-value-abcdefghijklmnopqrstuvwxyz"`,
		"opaque bearer":         `Authorization: Bearer mF_9.B5f-4.1JqM/opaque-token-value`,
		"json quoted key":       `{"api_key":"json-secret-value-12345"}`,
		"bare token key":        "TOKEN=opaque-live-secret-abcdefghijklmnopqrstuvwxyz",
		"namespaced token key":  "CI_JOB_TOKEN=opaque-live-secret-abcdefghijklmnopqrstuvwxyz",
		"hugging face token":    "hf_abcdefghijklmnopqrstuvwxyz0123456789",
		"npm token":             "npm_abcdefghijklmnopqrstuvwxyz0123456789",
		"basic authorization":   `Authorization: Basic QWxhZGRpbjpvcGVuIHNlc2FtZQ==`,
		"stripe secret key":     "sk_" + "live_abcdefghijklmnopqrstuvwxyz0123456789",
		"stripe restricted key": "rk_" + "live_abcdefghijklmnopqrstuvwxyz0123456789",
		"google api key":        "AIza" + strings.Repeat("A", 34) + "-",
		"namespaced secret key": "STRIPE_SECRET_KEY=opaque-live-secret-abcdefghijklmnopqrstuvwxyz",
		"private key variable":  "PRIVATE_KEY=ZmFrZS1wcml2YXRlLWtleS12YWx1ZQ==",
		"camel secret key":      "secretKey=opaque-live-secret-abcdefghijklmnopqrstuvwxyz",
		"camel private key":     "privateKey=opaque-private-material-abcdefghijklmnopqrstuvwxyz",
		"hyphen client secret":  "client-secret=opaque-client-secret-abcdefghijklmnopqrstuvwxyz",
		"hyphen private key":    "private-key=opaque-private-material-abcdefghijklmnopqrstuvwxyz",
		"namespaced api key":    "SERVICE_API_KEY=opaque-api-key-abcdefghijklmnopqrstuvwxyz",
		"camel api key":         "serviceApiKey=opaque-api-key-abcdefghijklmnopqrstuvwxyz",
		"namespaced password":   "DATABASE_PASSWORD=opaque-password-abcdefghijklmnopqrstuvwxyz",
		"camel passwd":          "dbPasswd=opaque-password-abcdefghijklmnopqrstuvwxyz",
		"camel auth secret":     "authSecret=opaque-auth-secret-abcdefghijklmnopqrstuvwxyz",
		"pgp private key":       "-----BEGIN PGP PRIVATE KEY BLOCK-----\nZmFrZS1wZ3AtcHJpdmF0ZS1rZXk=\n-----END PGP PRIVATE KEY BLOCK-----",
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

func TestSecretClassifierPreservesAnchoredCredentialPlaceholders(t *testing.T) {
	for _, value := range []string{
		"TOKEN=$TOKEN",
		"SERVICE_TOKEN=${SERVICE_TOKEN}",
		"HF_TOKEN=<hf_token>",
		"HF_TOKEN=hf_example",
		`NPM_TOKEN="{{ NPM_TOKEN }}"`,
		"NPM_TOKEN=npm_placeholder",
		"Authorization: Basic ${BASIC_AUTH}",
		`"Authorization": "Basic <basic_auth>"`,
		"STRIPE_SECRET_KEY=${STRIPE_SECRET_KEY}",
		"STRIPE_SECRET_KEY=sk_live_example",
		"PRIVATE_KEY=<private_key>",
		`SSH_PRIVATE_KEY="{{ SSH_PRIVATE_KEY }}"`,
		"SERVICE_SECRET_KEY=example_secret_key",
		"secretKey=${SECRET_KEY}",
		"privateKey=<private_key>",
		"client-secret=placeholder",
		`private-key="{{ PRIVATE_KEY }}"`,
		"SERVICE_API_KEY=${SERVICE_API_KEY}",
		"serviceApiKey=<api_key>",
		"DATABASE_PASSWORD=${DATABASE_PASSWORD}",
		"dbPasswd=placeholder",
		`authSecret="{{ AUTH_SECRET }}"`,
	} {
		if ContainsSecret(value) {
			t.Errorf("anchored placeholder classified as a credential: %q", value)
		}
		if got := RedactSecrets(value); got != value {
			t.Errorf("anchored placeholder changed: got %q want %q", got, value)
		}
	}
}

func TestSecretClassifierPreservesOnlyExactAnchoredRawTokenPlaceholders(t *testing.T) {
	placeholders := map[string]struct {
		value      string
		assignment string
	}{
		"hugging face": {
			value:      "hf_" + strings.Repeat("x", 20),
			assignment: "HF_TOKEN=hf_" + strings.Repeat("x", 20),
		},
		"npm": {
			value:      "npm_" + strings.Repeat("x", 20),
			assignment: "NPM_TOKEN=npm_" + strings.Repeat("x", 20),
		},
		"stripe": {
			value:      "sk_live_" + strings.Repeat("x", 16),
			assignment: "STRIPE_SECRET_KEY=sk_live_" + strings.Repeat("x", 16),
		},
	}
	for name, placeholder := range placeholders {
		t.Run(name, func(t *testing.T) {
			for _, exact := range []string{placeholder.value, placeholder.assignment} {
				if ContainsSecret(exact) {
					t.Errorf("exact anchored placeholder classified as a credential: %q", exact)
				}
				if got := RedactSecrets(exact); got != exact {
					t.Errorf("exact anchored placeholder changed: got %q want %q", got, exact)
				}
				if got := Text(exact); got != exact {
					t.Errorf("exact anchored placeholder changed by durable sanitization: got %q want %q", got, exact)
				}
			}

			for _, unsafe := range []string{
				"leaked " + placeholder.value,
				placeholder.value + "-real",
				placeholder.assignment + "-real",
			} {
				if !ContainsSecret(unsafe) {
					t.Errorf("embedded or adjacent token escaped classification: %q", unsafe)
					continue
				}
				redacted := RedactSecrets(unsafe)
				if !strings.Contains(redacted, "[REDACTED_SECRET]") || strings.Contains(Text(unsafe), placeholder.value) {
					t.Errorf("embedded or adjacent token escaped redaction: %q", redacted)
				}
			}
		})
	}
}

func TestSecretClassifierRejectsTokenPlaceholderSuffixes(t *testing.T) {
	for _, value := range []string{
		"TOKEN=$TOKEN-live",
		"SERVICE_TOKEN=${SERVICE_TOKEN}suffix",
		"HF_TOKEN=<hf_token>live",
		`NPM_TOKEN="{{ NPM_TOKEN }}suffix"`,
		"Authorization: Basic $BASIC_AUTH-live",
		"STRIPE_SECRET_KEY=sk_live_example-real",
		"PRIVATE_KEY=<private_key>live",
		"SERVICE_SECRET_KEY=${SERVICE_SECRET_KEY}suffix",
		"secretKey=${SECRET_KEY}suffix",
		"private-key=<private_key>live",
	} {
		if !ContainsSecret(value) {
			t.Errorf("ambiguous token placeholder was not classified: %q", value)
			continue
		}
		redacted := RedactSecrets(value)
		if !strings.Contains(redacted, "[REDACTED_SECRET]") || ContainsSecret(redacted) {
			t.Errorf("ambiguous token placeholder was not redacted fail-closed: %q", redacted)
		}
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
		"client_secret=placeholder_secret",
		"client_secret=your_client_secret",
		"client_secret=example-access-token",
		"password=$PASSWORD",
		"password=<password>",
		`password="{{ PASSWORD }}"`,
		"password=%PASSWORD%",
		`"refresh_token": "example"`,
		"Authorization: Bearer ${ACCESS_TOKEN}",
		"Authorization: Bearer <token>",
		"Authorization: Bearer example",
		"client_secret_hint=public-identifier",
		"-----BEGIN PUBLIC KEY-----",
		"sk_live_example",
		"rk_live_placeholder",
		"AIzaExampleKey",
		"PUBLIC_KEY=public-material",
		"SECRET_KEY_HINT=public-identifier",
		"PRIVATE_KEY_HINT=public-identifier",
		"secretKeyHint=public-identifier",
		"private-key-hint=public-identifier",
		"client-secret-hint=public-identifier",
		"publicKey=public-material",
		"SERVICE_API_KEY_HINT=public-identifier",
		"serviceApiKeyHint=public-identifier",
		"DATABASE_PASSWORD_HINT=public-identifier",
		"dbPasswdHint=public-identifier",
		"authSecretHint=public-identifier",
		"serviceApiDocumentation=public-identifier",
		"databasePasswordPolicy=public-identifier",
		"authSecretName=public-identifier",
	} {
		if ContainsSecret(value) {
			t.Errorf("safe lookalike classified as a credential: %q", value)
		}
		if got := RedactSecrets(value); got != value {
			t.Errorf("safe lookalike changed: got %q want %q", got, value)
		}
	}
}

func TestSecretClassifierRejectsAmbiguousPlaceholderPrefixes(t *testing.T) {
	for _, value := range []string{
		"password=$PASSWORD-extra",
		"client_secret=<token>live",
		`api_key="{{ API_KEY }}suffix"`,
		"access_token=%TOKEN%tail",
		"client_secret=placeholder-real-secret",
		"password=your_password_but_real",
		"password=",
		`password=""`,
		"Authorization: Bearer placeholder-real-secret",
		"Authorization: Bearer $TOKEN-extra",
		`{"Authorization": "Bearer actual-secret-value"}`,
		"STRIPE_SECRET_KEY=${STRIPE_SECRET_KEY}suffix",
		"PRIVATE_KEY=<private_key>live",
	} {
		if !ContainsSecret(value) {
			t.Errorf("ambiguous sensitive value was not classified: %q", value)
			continue
		}
		redacted := RedactSecrets(value)
		if !strings.Contains(redacted, "[REDACTED_SECRET]") || ContainsSecret(redacted) {
			t.Errorf("ambiguous sensitive value was not redacted fail-closed: %q", redacted)
		}
	}
}

func TestSecretRedactionConsumesEscapedAndMalformedQuotedValues(t *testing.T) {
	for _, value := range []string{
		`password="abc\"TOPSECRET"`,
		`password="abc\\\"TOPSECRET"`,
		`client_secret='abc\'TOPSECRET'`,
		`password="unterminated TOPSECRET`,
		`Authorization: "Bearer abc\"TOPSECRET"`,
		`Authorization: "Bearer unterminated TOPSECRET`,
	} {
		if !ContainsSecret(value) {
			t.Errorf("quoted sensitive value was not classified: %q", value)
			continue
		}
		redacted := RedactSecrets(value)
		if strings.Contains(redacted, "TOPSECRET") || !strings.Contains(redacted, "[REDACTED_SECRET]") {
			t.Errorf("quoted sensitive suffix survived redaction: %q", redacted)
		}
	}
}

func TestSecretClassifierRedactsCredentialURIAndMultilineEncodings(t *testing.T) {
	tests := map[string]struct {
		value     string
		forbidden []string
	}{
		"postgres DSN userinfo": {
			value:     "DATABASE_URL=postgres://alice:correcthorse@127.0.0.1/app",
			forbidden: []string{"correcthorse"},
		},
		"redis DSN userinfo": {
			value:     "REDIS_URL=redis://:redispassword@localhost/0",
			forbidden: []string{"redispassword"},
		},
		"percent-encoded URI password": {
			value:     "DATABASE_URL=postgres://alice:p%40ssword@localhost/app?sslmode=require",
			forbidden: []string{"p%40ssword"},
		},
		"MySQL TCP DSN": {
			value:     "DATABASE_URL=alice:mysqlpassword@tcp(localhost:3306)/app",
			forbidden: []string{"mysqlpassword"},
		},
		"MySQL Unix DSN": {
			value:     "MYSQL_DSN=alice:unixpassword@unix(/var/run/mysql.sock)/app",
			forbidden: []string{"unixpassword"},
		},
		"ODBC PWD": {
			value:     "ODBC_DSN=Driver={PostgreSQL};UID=alice;PWD=odbcpassword;Server=localhost",
			forbidden: []string{"odbcpassword"},
		},
		"ODBC braced PWD": {
			value:     "Driver={PostgreSQL};PWD={braced password};Server=localhost",
			forbidden: []string{"braced password"},
		},
		"YAML literal scalar": {
			value:     "password: |-\n  yamlsecretvalue\nPUBLIC_URL: https://example.test/docs",
			forbidden: []string{"yamlsecretvalue"},
		},
		"YAML folded scalar": {
			value:     "client_secret: >+\n  foldedsecretvalue\n  secondsecretline",
			forbidden: []string{"foldedsecretvalue", "secondsecretline"},
		},
		"YAML plain multiline scalar": {
			value:     "password: firstsecretline\n  secondsecretline\nPUBLIC_VALUE: safe",
			forbidden: []string{"firstsecretline", "secondsecretline"},
		},
		"YAML implicit sequence scalar": {
			value:     "password:\n  - listsecretvalue\nPUBLIC_VALUE: safe",
			forbidden: []string{"listsecretvalue"},
		},
		"malformed unindented YAML scalar": {
			value:     "password: |-\nmalformedsecretvalue\nPUBLIC_VALUE: safe",
			forbidden: []string{"malformedsecretvalue"},
		},
		"nested YAML list scalar with tag and indent indicator": {
			value:     "databases:\n  - password: !!str |2- # credential\n      nestedsecretvalue\n  - PUBLIC_URL: https://example.test/docs",
			forbidden: []string{"nestedsecretvalue"},
		},
		"continued assignment": {
			value:     "TOKEN=continued\\\nsecretvalue\nPUBLIC_VALUE=safe",
			forbidden: []string{"continued", "secretvalue"},
		},
		"inline continued assignment": {
			value:     "Use TOKEN=continued\\\nsecretvalue\nPUBLIC_VALUE=safe",
			forbidden: []string{"continued", "secretvalue"},
		},
		"inline YAML scalar": {
			value:     "Use password: |-\ninlineyamlsecret\nPUBLIC_VALUE=safe",
			forbidden: []string{"inlineyamlsecret"},
		},
		"exported multi-continuation assignment": {
			value:     "export TOKEN=first\\\n  second\\\n  third\nPUBLIC_VALUE=safe",
			forbidden: []string{"first", "second", "third"},
		},
		"Authorization continuation": {
			value:     "Authorization: Bearer firstbearersecret\\\nsecondbearersecret\nPUBLIC_VALUE=safe",
			forbidden: []string{"firstbearersecret", "secondbearersecret"},
		},
		"Authorization plain multiline": {
			value:     "Authorization: Basic firstbasicsecret\n  secondbasicsecret\nPUBLIC_VALUE=safe",
			forbidden: []string{"firstbasicsecret", "secondbasicsecret"},
		},
		"multiline quoted assignment": {
			value:     "password=\"firstsecretline\n  secondsecretline\"\nPUBLIC_VALUE=safe",
			forbidden: []string{"firstsecretline", "secondsecretline"},
		},
		"CRLF YAML scalar": {
			value:     "password: |-\r\n  crlfsecretvalue\r\nPUBLIC_VALUE=safe",
			forbidden: []string{"crlfsecretvalue"},
		},
		"placeholder suffix in URI": {
			value:     "DATABASE_URL=postgres://alice:${DATABASE_PASSWORD}-real@localhost/app",
			forbidden: []string{"${DATABASE_PASSWORD}-real"},
		},
		"placeholder suffix in YAML": {
			value:     "password: |-\n  ${PASSWORD}-real\nPUBLIC_VALUE=safe",
			forbidden: []string{"${PASSWORD}-real"},
		},
		"raw placeholder suffix in YAML": {
			value:     "password: |-\n  hf_" + strings.Repeat("x", 20) + "-real\nPUBLIC_VALUE=safe",
			forbidden: []string{"hf_" + strings.Repeat("x", 20)},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if !ContainsSecret(test.value) {
				t.Fatalf("credential encoding was not classified: %q", test.value)
			}
			redacted := RedactSecrets(test.value)
			if !strings.Contains(redacted, "[REDACTED_SECRET]") || ContainsSecret(redacted) {
				t.Fatalf("credential encoding was not completely redacted: %q", redacted)
			}
			if repeated := RedactSecrets(redacted); repeated != redacted {
				t.Fatalf("credential redaction was not idempotent: first %q, second %q", redacted, repeated)
			}
			for _, forbidden := range test.forbidden {
				for label, persisted := range map[string]string{
					"redacted":    redacted,
					"text":        Text(test.value),
					"fingerprint": Fingerprint(test.value),
				} {
					if strings.Contains(persisted, forbidden) {
						t.Errorf("%s retained secret fragment %q in %q", label, forbidden, persisted)
					}
				}
			}
			if strings.Contains(test.value, "PUBLIC_") && !strings.Contains(redacted, "PUBLIC_") {
				t.Errorf("redaction consumed the following public field: %q", redacted)
			}
		})
	}
}

func TestSecretClassifierPreservesBenignURIsMultilineHintsAndPlaceholders(t *testing.T) {
	for _, value := range []string{
		"DATABASE_URL=postgres://localhost/app",
		"DATABASE_URL=postgres://localhost:5432/app",
		"DATABASE_URL=postgres://alice@localhost/app",
		"DOCS_URL=https://alice@example.test/docs",
		"https://example.test/path",
		"https://example.test:443/path",
		"DATABASE_URL=postgres://alice:${DATABASE_PASSWORD}@localhost/app",
		"DATABASE_URL=postgres://alice:placeholder@localhost/app",
		"DATABASE_URL_HINT=postgres://alice:example@localhost/app",
		"DATABASE_URL=alice@tcp(localhost:3306)/app",
		"DATABASE_URL=alice:${DATABASE_PASSWORD}@tcp(localhost:3306)/app",
		"DATABASE_URL=alice:placeholder@unix(/var/run/mysql.sock)/app",
		"DATABASE_URL_HINT=alice:public-identifier@tcp(localhost:3306)/app",
		"CONTACT=alice:public-identifier@tcp(localhost:3306)/app",
		"PWD=/safe/current/directory",
		"ODBC_DSN=Driver={PostgreSQL};UID=alice;PWD=${DATABASE_PASSWORD};Server=localhost",
		"ODBC_DSN_HINT=Driver={PostgreSQL};UID=alice;PWD=public-identifier;Server=localhost",
		"password: |-\n  ${PASSWORD}",
		"TOKEN=\\\n${TOKEN}",
		"Use TOKEN=\\\n${TOKEN}",
		"password: |-\n  hf_" + strings.Repeat("x", 20),
		"TOKEN=\\\nnpm_" + strings.Repeat("x", 20),
		"STRIPE_SECRET_KEY=\\\nsk_live_" + strings.Repeat("x", 16),
		"PUBLIC_KEY: |-\n  public-key-material",
		"PRIVATE_KEY_HINT: >-\n  public-identifier",
	} {
		if ContainsSecret(value) {
			t.Errorf("benign URI, hint, public value, or placeholder classified as a credential: %q", value)
		}
		if got := RedactSecrets(value); got != value {
			t.Errorf("benign URI, hint, public value, or placeholder changed: got %q want %q", got, value)
		}
	}
}

func TestSecretClassifierHandlesManyMultilineRawPlaceholders(t *testing.T) {
	input := manyMultilineRawPlaceholders(10_000)
	if ContainsSecret(input) {
		t.Fatal("exact multiline placeholders were classified as credentials")
	}
	if got := RedactSecrets(input); got != input {
		t.Fatal("exact multiline placeholders changed during redaction")
	}
}

func BenchmarkSecretClassifierManyMultilineRawPlaceholders(b *testing.B) {
	for name, count := range map[string]int{"1k": 1_000, "4k": 4_000, "16k": 16_000} {
		input := manyMultilineRawPlaceholders(count)
		b.Run(name+"/contains", func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			for range b.N {
				_ = ContainsSecret(input)
			}
		})
		b.Run(name+"/redact", func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			for range b.N {
				_ = RedactSecrets(input)
			}
		})
	}
}

func manyMultilineRawPlaceholders(count int) string {
	var input strings.Builder
	for index := range count {
		input.WriteString("password: |-\n  hf_")
		input.WriteString(strings.Repeat("x", 20))
		input.WriteString("\nPUBLIC_VALUE_")
		input.WriteString(string(rune('a' + index%26)))
		input.WriteString(": safe\n")
	}
	return input.String()
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
