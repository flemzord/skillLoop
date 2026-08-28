package learning

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flemzord/skillloop/internal/domain"
)

const goTestSuccessOutput = "ok\tgithub.com/example/project\t0.123s"

func TestAnalyzeAttributedCorrectionAndFailure(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceCodex, ExternalID: "session-1",
		Messages: append(codexInstructionRead("/skills/go-service/SKILL.md"),
			domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "call-1", Text: `{"cmd":"go test ./..."}`},
			domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "call-1", ToolResult: true, Text: "exit code 1: package failed", Failed: true},
			domain.Message{Role: "user", Text: "Non, il faut utiliser nix develop."},
			domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "call-2", Text: `{"cmd":"nix develop --command go test ./..."}`},
			domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "call-2", ToolResult: true, Text: "ok"},
		),
	}
	skills := []domain.Skill{{ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service", InstructionPath: "SKILL.md", Enabled: true}}
	cards := NewAnalyzer().Analyze(session, skills)
	if len(cards) != 2 {
		t.Fatalf("expected failure and correction cards, got %#v", cards)
	}
	if cards[0].SessionRef != "codex:session-1" || cards[0].SkillID != "skill-1" {
		t.Fatalf("unexpected attribution: %#v", cards[0])
	}
	if cards[0].Kind != domain.CardFailure || contains(cards[0].Lesson, "validated recovery") {
		t.Fatalf("repo-controlled nix shell was accepted as recovery proof: %#v", cards[0])
	}
	if cards[1].Kind != domain.CardCorrection {
		t.Fatalf("expected user correction, got %#v", cards[1])
	}
}

func TestAnalyzeIgnoresInjectedAgentInstructionsAsCorrections(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceCodex, ExternalID: "injected-context",
		Messages: append(codexInstructionRead("/skills/go-service/SKILL.md"),
			domain.Message{Role: "user", Text: "# AGENTS.md instructions\n\n<INSTRUCTIONS>\n- Tu dois lancer les tests.\n</INSTRUCTIONS>\n<environment_context>...</environment_context>"},
		),
	}
	skill := domain.Skill{ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service", InstructionPath: "SKILL.md", Enabled: true}
	if cards := NewAnalyzer().Analyze(session, []domain.Skill{skill}); len(cards) != 0 {
		t.Fatalf("injected instructions produced learning cards: %#v", cards)
	}
}

func TestAnalyzeRedactsExpandedSecretFamilies(t *testing.T) {
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	credentials := map[string]string{
		"bare token key":        "TOKEN=opaque-live-secret-abcdefghijklmnopqrstuvwxyz",
		"namespaced token key":  "CI_JOB_TOKEN=opaque-live-secret-abcdefghijklmnopqrstuvwxyz",
		"hugging face":          "hf_abcdefghijklmnopqrstuvwxyz0123456789",
		"npm":                   "npm_abcdefghijklmnopqrstuvwxyz0123456789",
		"basic authorization":   "Authorization: Basic QWxhZGRpbjpvcGVuIHNlc2FtZQ==",
		"stripe secret":         "sk_" + "live_abcdefghijklmnopqrstuvwxyz0123456789",
		"stripe restricted":     "rk_" + "live_abcdefghijklmnopqrstuvwxyz0123456789",
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
	for name, credential := range credentials {
		t.Run(name, func(t *testing.T) {
			session := domain.Session{
				Source: domain.SourceCodex, ExternalID: "expanded-secret-" + name,
				Messages: append(codexInstructionRead("/skills/go-service/SKILL.md"),
					domain.Message{Role: "user", Text: "No, use " + credential + " for this request."},
				),
			}
			cards := NewAnalyzer().Analyze(session, []domain.Skill{skill})
			if len(cards) != 1 || cards[0].Kind != domain.CardCorrection {
				t.Fatalf("expanded secret correction produced %#v", cards)
			}
			persistable := cards[0].Summary + " " + cards[0].Lesson + " " + cards[0].Fingerprint
			if strings.Contains(persistable, credential) {
				t.Fatalf("credential survived analyzer boundary: %q", persistable)
			}
			if !strings.Contains(cards[0].Lesson, "[REDACTED_SECRET]") {
				t.Fatalf("analyzer did not mark the redaction: %#v", cards[0])
			}
		})
	}
}

func TestAnalyzeRedactsCredentialEncodingsFromDurableFields(t *testing.T) {
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	tests := map[string]struct {
		correction string
		forbidden  []string
	}{
		"URI userinfo": {
			correction: "No, use DATABASE_URL=postgres://alice:correcthorse@localhost/app for this request.",
			forbidden:  []string{"correcthorse"},
		},
		"MySQL DSN": {
			correction: "No, use DATABASE_URL=alice:mysqlpassword@tcp(localhost:3306)/app for this request.",
			forbidden:  []string{"mysqlpassword"},
		},
		"ODBC PWD": {
			correction: "No, use ODBC_DSN=Driver={PostgreSQL};UID=alice;PWD=odbcpassword;Server=localhost.",
			forbidden:  []string{"odbcpassword"},
		},
		"YAML block scalar": {
			correction: "No, use this configuration:\npassword: |-\n  yamlsecretvalue\nPUBLIC_URL: https://example.test/docs",
			forbidden:  []string{"yamlsecretvalue"},
		},
		"malformed YAML block scalar": {
			correction: "No, use this configuration:\npassword: |-\nmalformedsecretvalue\nPUBLIC_VALUE: safe",
			forbidden:  []string{"malformedsecretvalue"},
		},
		"YAML plain multiline scalar": {
			correction: "No, use this configuration:\npassword: firstsecretline\n  secondsecretline\nPUBLIC_VALUE: safe",
			forbidden:  []string{"firstsecretline", "secondsecretline"},
		},
		"continued assignment": {
			correction: "No, use this configuration:\nTOKEN=continued\\\nsecretvalue\nPUBLIC_VALUE=safe",
			forbidden:  []string{"continued", "secretvalue"},
		},
		"Authorization continuation": {
			correction: "No, use this header:\nAuthorization: Bearer firstbearersecret\\\nsecondbearersecret\nPUBLIC_VALUE: safe",
			forbidden:  []string{"firstbearersecret", "secondbearersecret"},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			session := domain.Session{
				Source: domain.SourceCodex, ExternalID: "encoded-secret-" + name,
				Messages: append(codexInstructionRead("/skills/go-service/SKILL.md"),
					domain.Message{Role: "user", Text: test.correction},
				),
			}
			cards := NewAnalyzer().Analyze(session, []domain.Skill{skill})
			if len(cards) != 1 || cards[0].Kind != domain.CardCorrection {
				t.Fatalf("encoded secret correction produced %#v", cards)
			}
			persistable := cards[0].Summary + " " + cards[0].Lesson + " " + cards[0].Fingerprint
			for _, forbidden := range test.forbidden {
				if strings.Contains(persistable, forbidden) {
					t.Fatalf("credential fragment %q survived analyzer boundary: %q", forbidden, persistable)
				}
			}
			if !strings.Contains(cards[0].Lesson, "[REDACTED_SECRET]") {
				t.Fatalf("analyzer did not mark the redaction: %#v", cards[0])
			}
		})
	}
}

func TestAnalyzeFailedValidationCallDoesNotCreateValidation(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceClaude, ExternalID: "session-failed-validation",
		Messages: append(claudeInstructionRead("/skills/go-service/SKILL.md"),
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "tool-1", Text: `{"command":"go test ./..."}`},
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "tool-1", ToolResult: true, Text: "FAIL\nexit status 1", Failed: true},
		),
	}
	skills := []domain.Skill{{ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service", InstructionPath: "SKILL.md", Enabled: true}}
	cards := NewAnalyzer().Analyze(session, skills)
	if len(cards) != 1 || cards[0].Kind != domain.CardFailure {
		t.Fatalf("expected only a failure card after a failed validation result, got %#v", cards)
	}
}

func TestAnalyzeIgnoresFailedToolCallWithoutResult(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceCodex, ExternalID: "session-call-only",
		Messages: append(codexInstructionRead("/skills/go-service/SKILL.md"),
			domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "call-1", Text: `{"cmd":"go test ./..."}`, Failed: true},
		),
	}
	skills := []domain.Skill{{ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service", InstructionPath: "SKILL.md", Enabled: true}}
	if cards := NewAnalyzer().Analyze(session, skills); len(cards) != 0 {
		t.Fatalf("expected tool calls without results to produce no cards, got %#v", cards)
	}
}

func TestAnalyzeDoesNotInferValidationFromResultText(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceClaude, ExternalID: "session-result-text",
		Messages: append(claudeInstructionRead("/skills/go-service/SKILL.md"),
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "tool-1", Text: `{"command":"echo done"}`},
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "tool-1", ToolResult: true, Text: "go test passed"},
		),
	}
	skills := []domain.Skill{{ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service", InstructionPath: "SKILL.md", Enabled: true}}
	if cards := NewAnalyzer().Analyze(session, skills); len(cards) != 0 {
		t.Fatalf("expected validation detection to inspect the associated call, got %#v", cards)
	}
}

func TestAnalyzeValidationRequiresAnExactStructuredCommand(t *testing.T) {
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	tests := []struct {
		name     string
		source   domain.Source
		toolName string
		payload  string
	}{
		{name: "echo quoted validation", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"echo 'go test ./...'"}`},
		{name: "shell comment", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"go test ./... # claimed validation"}`},
		{name: "composed and", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"go test ./... && true"}`},
		{name: "composed newline", source: domain.SourceCodex, toolName: "exec_command", payload: "{\"cmd\":\"go test ./...\\ntrue\"}"},
		{name: "nested composed command", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"sh -c 'go test ./...; true'"}`},
		{name: "wrong executable", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"go-test ./..."}`},
		{name: "substring executable", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"go testhelper ./..."}`},
		{name: "golangci help is not validation", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"golangci-lint --help"}`},
		{name: "nix wrapper runs echo", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"nix develop --command echo go test"}`},
		{name: "raw unstructured command", source: domain.SourceCodex, toolName: "exec_command", payload: `go test ./...`},
		{name: "wrong codex field", source: domain.SourceCodex, toolName: "exec_command", payload: `{"command":"go test ./..."}`},
		{name: "sudo option consumes apparent command", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"sudo -u go test"}`},
		{name: "env option consumes apparent command", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"env -u go test"}`},
		{name: "forged codex shell name", source: domain.SourceCodex, toolName: "Bash", payload: `{"command":"go test ./..."}`},
		{name: "forged claude shell name", source: domain.SourceClaude, toolName: "exec_command", payload: `{"cmd":"go test ./..."}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			messages := codexInstructionRead("/skills/go-service/SKILL.md")
			if test.source == domain.SourceClaude {
				messages = claudeInstructionRead("/skills/go-service/SKILL.md")
			}
			messages = append(messages,
				domain.Message{Role: "tool", ToolName: test.toolName, ToolCallID: "validation", Text: test.payload},
				domain.Message{Role: "tool", ToolName: test.toolName, ToolCallID: "validation", ToolResult: true, Text: "ok"},
			)
			session := domain.Session{Source: test.source, ExternalID: test.name, Messages: messages}
			if cards := NewAnalyzer().Analyze(session, []domain.Skill{skill}); len(cards) != 0 {
				t.Fatalf("untrusted validation command created cards: %#v", cards)
			}
		})
	}
}

func TestAnalyzeAcceptsSupportedValidationCommandsAndSafeWrappers(t *testing.T) {
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	commands := map[string]struct {
		command string
		lesson  string
		output  string
	}{
		"go":                {command: "go test ./...", lesson: "go test ./...", output: goTestSuccessOutput},
		"go targeted":       {command: "go test -v -run TestThing ./...", lesson: "go test -v -run TestThing ./...", output: "=== RUN   TestThing\n--- PASS: TestThing (0.00s)\nPASS\n" + goTestSuccessOutput},
		"go skip selector":  {command: "go test -v -skip TestSlow ./...", lesson: "go test -v -skip TestSlow ./...", output: "=== RUN   TestThing\n--- PASS: TestThing (0.00s)\nPASS\n" + goTestSuccessOutput},
		"go race count":     {command: "go test -race -count=1 ./...", lesson: "go test -race -count=1 ./...", output: goTestSuccessOutput},
		"go benchmark":      {command: "go test -run a^ -bench=. ./...", lesson: "go test -run a^ -bench=. ./...", output: "BenchmarkThing-8\t100\t12 ns/op\nPASS\n" + goTestSuccessOutput},
		"go mixed selector": {command: "go test -v -run TestThing -bench=a^ ./...", lesson: "go test -v -run TestThing -bench=a^ ./...", output: "=== RUN   TestThing\n--- PASS: TestThing (0.00s)\nPASS\n" + goTestSuccessOutput},
		"go fuzz":           {command: "go test -fuzz=FuzzThing -fuzztime=1s ./internal/foo", lesson: "go test -fuzz=FuzzThing -fuzztime=1s ./internal/foo", output: "fuzz: elapsed: 1s, execs: 42 (42/sec), new interesting: 1 (total: 2)\nPASS\n" + goTestSuccessOutput},
		"go json":           {command: "go test -json ./...", lesson: "go test -json ./...", output: `{"Action":"pass","Package":"github.com/example/project","Test":"TestThing"}`},
		"go test args":      {command: "go test ./... -args -test.v -test.run TestThing", lesson: "go test ./... -args -test.v -test.run TestThing", output: "=== RUN   TestThing\n--- PASS: TestThing (0.00s)\nPASS\n" + goTestSuccessOutput},
		"golangci lint":     {command: "golangci-lint run --timeout 5m", lesson: "golangci-lint run --timeout 5m"},
		"nix flake":         {command: "nix flake check path:.", lesson: "nix flake check path:."},
		"pytest":            {command: "pytest -q", lesson: "pytest -q"},
		"pytest verbose":    {command: "pytest -v", lesson: "pytest -v"},
		"cargo":             {command: "cargo test --workspace", lesson: "cargo test --workspace"},
		"npm":               {command: "npm test -- --runInBand", lesson: "npm test -- --runInBand"},
		"pnpm":              {command: "pnpm test", lesson: "pnpm test"},
		"just":              {command: "just test", lesson: "just test"},
		"env wrapper":       {command: "env CGO_ENABLED=0 go test ./...", lesson: "env CGO_ENABLED=0 go test ./...", output: goTestSuccessOutput},
		"command wrapper":   {command: "command -- go test ./...", lesson: "go test ./...", output: goTestSuccessOutput},
	}
	for name, test := range commands {
		t.Run(name, func(t *testing.T) {
			messages := append(codexInstructionRead("/skills/go-service/SKILL.md"),
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "validation", Text: `{"cmd":` + quotedJSON(t, test.command) + `}`},
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "validation", ToolResult: true, Text: validationOutput(test.output)},
			)
			session := domain.Session{Source: domain.SourceCodex, ExternalID: name, Messages: messages}
			cards := NewAnalyzer().Analyze(session, []domain.Skill{skill})
			if len(cards) != 1 || cards[0].Kind != domain.CardValidation || cards[0].Lesson != test.lesson {
				t.Fatalf("supported validation command produced %#v", cards)
			}
		})
	}

	messages := append(claudeInstructionRead("/skills/go-service/SKILL.md"),
		domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "validation", Text: `{"command":"go test ./..."}`},
		domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "validation", ToolResult: true, Text: goTestSuccessOutput},
	)
	session := domain.Session{Source: domain.SourceClaude, ExternalID: "claude-bash", Messages: messages}
	if cards := NewAnalyzer().Analyze(session, []domain.Skill{skill}); len(cards) != 1 || cards[0].Kind != domain.CardValidation {
		t.Fatalf("Claude Bash validation produced %#v", cards)
	}
}

func TestAnalyzeRejectsNonExecutingValidationModes(t *testing.T) {
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	commands := map[string]string{
		"go external no-op":       "go test -exec=true ./...",
		"go external no-op value": "go test -exec true ./...",
		"go tool executor":        "go test -toolexec=true ./...",
		"go tool executor value":  "go test -toolexec true ./...",
		"go compile only":         "go test -c",
		"go print only":           "go test -n ./...",
		"go list only":            "go test -list . ./...",
		"go empty selection":      "go test -run a^ ./...",
		"go zero count":           "go test -count=0 ./...",
		"go unknown option":       "go test -unrecognized ./...",
		"go list after args":      "go test ./... -args -test.list .",
		"golangci help":           "golangci-lint run --help",
		"golangci assigned help":  "golangci-lint run --help=true",
		"golangci forced success": "golangci-lint run --issues-exit-code=0",
		"nix no build":            "nix flake check --no-build",
		"pytest collect only":     "pytest --collect-only",
		"pytest setup plan":       "pytest --setup-plan",
		"cargo no run":            "cargo test --no-run",
		"cargo list only":         "cargo test -- --list",
		"npm ignores scripts":     "npm test --ignore-scripts",
		"npm missing is success":  "npm test --if-present",
		"pnpm dry run":            "pnpm test --dry-run",
		"runner help":             "just test --help",
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			output := "ok"
			if strings.HasPrefix(command, "go test") {
				output = goTestSuccessOutput
			}
			if name == "go empty selection" {
				output = "testing: warning: no tests to run\nPASS\n" +
					"ok\tgithub.com/example/project\t0.001s [no tests to run]"
			}
			messages := append(codexInstructionRead("/skills/go-service/SKILL.md"),
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "validation", Text: `{"cmd":` + quotedJSON(t, command) + `}`},
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "validation", ToolResult: true, Text: output},
			)
			session := domain.Session{Source: domain.SourceCodex, ExternalID: name, Messages: messages}
			if cards := NewAnalyzer().Analyze(session, []domain.Skill{skill}); len(cards) != 0 {
				t.Fatalf("non-executing validation mode created cards: %#v", cards)
			}
		})
	}
}

func TestAnalyzeGoValidationRequiresExecutedTestEvidence(t *testing.T) {
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	tests := map[string]struct {
		command string
		output  string
	}{
		"empty result":         {command: "go test -run DoesNotExist ./...", output: ""},
		"generic ok":           {command: "go test -run DoesNotExist ./...", output: "ok"},
		"truncated package ok": {command: "go test -run DoesNotExist ./...", output: "ok\tgithub.com/example/project"},
		"no test files":        {command: "go test -run DoesNotExist ./...", output: "?\tgithub.com/example/project\t[no test files]"},
		"unmatched run":        {command: "go test -run DoesNotExist ./...", output: "testing: warning: no tests to run\nPASS\nok\tgithub.com/example/project\t0.001s [no tests to run]"},
		"unmatched run package success": {
			command: "go test -run a^ ./internal/learning",
			output:  goTestSuccessOutput,
		},
		"skip all package success": {
			command: "go test -skip . ./internal/learning",
			output:  goTestSuccessOutput,
		},
		"unmatched test and benchmark": {
			command: "go test -run a^ -bench a^ ./internal/learning",
			output:  "PASS\nok  \tgithub.com/flemzord/skillloop/internal/learning\t0.359s",
		},
		"unmatched test and benchmark after args": {
			command: "go test ./internal/learning -args -test.run=a^ -test.bench=a^",
			output:  "PASS\nok  \tgithub.com/flemzord/skillloop/internal/learning\t0.359s",
		},
		"package-only JSON":    {command: "go test -run DoesNotExist ./...", output: `{"Action":"pass","Package":"github.com/example/project"}`},
		"skipped test JSON":    {command: "go test -run DoesNotExist ./...", output: `{"Action":"skip","Package":"github.com/example/project","Test":"TestThing"}`},
		"truncated PASS claim": {command: "go test -run DoesNotExist ./...", output: "PASS"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			messages := append(codexInstructionRead("/skills/go-service/SKILL.md"),
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "validation", Text: `{"cmd":` + quotedJSON(t, test.command) + `}`},
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "validation", ToolResult: true, Text: test.output},
			)
			cards := NewAnalyzer().Analyze(
				domain.Session{Source: domain.SourceCodex, ExternalID: name, Messages: messages},
				[]domain.Skill{skill},
			)
			if len(cards) != 0 {
				t.Fatalf("non-executed go test result created cards: %#v", cards)
			}
		})
	}
}

func TestAnalyzeValidationFingerprintBindsMaterialInvocation(t *testing.T) {
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	cardFor := func(sessionID, command string) domain.LearningCard {
		t.Helper()
		messages := append(codexInstructionRead("/skills/go-service/SKILL.md"),
			domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "validation", Text: `{"cmd":` + quotedJSON(t, command) + `}`},
			domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "validation", ToolResult: true, Text: goTestSuccessOutput},
		)
		cards := NewAnalyzer().Analyze(domain.Session{Source: domain.SourceCodex, ExternalID: sessionID, Messages: messages}, []domain.Skill{skill})
		if len(cards) != 1 || cards[0].Kind != domain.CardValidation {
			t.Fatalf("validation command produced %#v", cards)
		}
		return cards[0]
	}

	baseline := cardFor("baseline", "go test ./...")
	whitespace := cardFor("whitespace", "go   test   ./...")
	commandWrapper := cardFor("command-wrapper", "command -- go test ./...")
	short := cardFor("short", "go test -short ./...")
	packageScope := cardFor("package", "go test ./internal/learning")
	environment := cardFor("environment", "env CGO_ENABLED=0 go test ./...")

	for name, concordant := range map[string]domain.LearningCard{"whitespace": whitespace, "command wrapper": commandWrapper} {
		if concordant.Fingerprint != baseline.Fingerprint {
			t.Errorf("%s split equivalent validation proof: %q != %q", name, concordant.Fingerprint, baseline.Fingerprint)
		}
	}
	for name, distinct := range map[string]domain.LearningCard{"flag": short, "package": packageScope, "environment": environment} {
		if distinct.Fingerprint == baseline.Fingerprint {
			t.Errorf("materially different %s inherited validation fingerprint %q", name, baseline.Fingerprint)
		}
	}
}

func TestAnalyzeRejectsValidationExecutablesAcceptedOnlyByBasename(t *testing.T) {
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	symlink := filepath.Join(t.TempDir(), "go")
	if err := os.Symlink("go", symlink); err != nil {
		t.Fatal(err)
	}
	commands := map[string]string{
		"relative executable":       "./go test ./...",
		"relative executable path":  "tools/go test ./...",
		"absolute executable":       "/tmp/go test ./...",
		"symlinked executable":      symlink + " test ./...",
		"path override":             "env PATH=/tmp go test ./...",
		"relative shell wrapper":    "./sh -c 'go test ./...'",
		"absolute env wrapper":      "/usr/bin/env CGO_ENABLED=0 go test ./...",
		"relative nested command":   "nix develop path:. --command ./go test ./...",
		"repo controlled nix shell": "nix develop path:. --command go test ./...",
		"shell wrapper":             "bash -c 'go test ./...'",
		"sudo wrapper":              "sudo -n go test ./...",
		"bash startup":              "env BASH_ENV=/repo/preload bash -c 'go test ./...'",
		"posix shell startup":       "env ENV=/repo/preload sh -c 'go test ./...'",
		"shell options":             "env SHELLOPTS=errexit bash -c 'go test ./...'",
		"command lookup directory":  "env CDPATH=/repo bash -c 'go test ./...'",
		"glob behavior":             "env GLOBIGNORE=go bash -c 'go test ./...'",
		"prompt hook":               "env PROMPT_COMMAND=/repo/preload bash -c 'go test ./...'",
		"go environment":            "env GOENV=/repo/goenv go test ./...",
		"go flags":                  "env GOFLAGS=-toolexec=fake go test ./...",
		"go toolchain":              "env GOTOOLCHAIN=local go test ./...",
		"dynamic loader":            "env LD_PRELOAD=/repo/preload.so go test ./...",
		"darwin dynamic loader":     "env DYLD_INSERT_LIBRARIES=/repo/preload.dylib go test ./...",
		"home environment":          "env HOME=/repo/home go test ./...",
		"unknown environment":       "env PROJECT_HOOK=/repo/preload go test ./...",
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			messages := append(codexInstructionRead("/skills/go-service/SKILL.md"),
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "validation", Text: `{"cmd":` + quotedJSON(t, command) + `}`},
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "validation", ToolResult: true, Text: "ok"},
			)
			session := domain.Session{Source: domain.SourceCodex, ExternalID: name, Messages: messages}
			if cards := NewAnalyzer().Analyze(session, []domain.Skill{skill}); len(cards) != 0 {
				t.Fatalf("untrusted executable created cards: %#v", cards)
			}
		})
	}
}

func TestAnalyzeAcceptsTrustedCodexReadCommandsAndSafeWrappers(t *testing.T) {
	const instruction = "/skills/go-service/SKILL.md"
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	commands := map[string]string{
		"direct":              "sed -n '1,240p' '" + instruction + "'",
		"documented options":  "sed --posix -n '1,240p' '" + instruction + "'",
		"command wrapper":     "command -- sed -n '1,240p' '" + instruction + "'",
		"environment wrapper": "env LC_ALL=C sed -n '1,240p' '" + instruction + "'",
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			messages := []domain.Message{
				{Role: "tool", ToolName: "exec_command", ToolCallID: "read", Text: `{"cmd":` + quotedJSON(t, command) + `}`},
				{Role: "tool", ToolName: "exec_command", ToolCallID: "read", ToolResult: true, Text: "skill instructions"},
				{Role: "user", Text: "No, instead run the tests"},
			}
			session := domain.Session{Source: domain.SourceCodex, ExternalID: name, Messages: messages}
			cards := NewAnalyzer().Analyze(session, []domain.Skill{skill})
			if len(cards) != 1 || cards[0].Kind != domain.CardCorrection {
				t.Fatalf("trusted read command produced %#v", cards)
			}
		})
	}

	t.Run("non-empty Codex output envelope", func(t *testing.T) {
		messages := []domain.Message{
			{Role: "tool", ToolName: "exec_command", ToolCallID: "read", Text: `{"cmd":"sed -n '1,240p' '/skills/go-service/SKILL.md'"}`},
			{
				Role: "tool", ToolName: "exec_command", ToolCallID: "read", ToolResult: true,
				Text: "Process exited with code 0\nFinal output:\nskill instructions",
			},
			{Role: "user", Text: "No, instead run the tests"},
		}
		cards := NewAnalyzer().Analyze(
			domain.Session{Source: domain.SourceCodex, ExternalID: "wrapped-read", Messages: messages},
			[]domain.Skill{skill},
		)
		if len(cards) != 1 || cards[0].Kind != domain.CardCorrection {
			t.Fatalf("non-empty Codex read envelope produced %#v", cards)
		}
	})
}

func TestAnalyzeRejectsSedMetadataUnknownOptionsAndEmptyOutput(t *testing.T) {
	const instruction = "/skills/go-service/SKILL.md"
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	tests := map[string]struct {
		command string
		output  string
	}{
		"help":            {command: "sed --help -n '1,240p' '" + instruction + "'", output: "Usage: sed [OPTION]..."},
		"version":         {command: "sed --version -n '1,240p' '" + instruction + "'", output: "sed 4.9"},
		"unknown long":    {command: "sed --definitely-unknown -n '1,240p' '" + instruction + "'", output: "skill instructions"},
		"unknown short":   {command: "sed -X -n '1,240p' '" + instruction + "'", output: "skill instructions"},
		"in-place suffix": {command: "sed -i.bak -n '1,240p' '" + instruction + "'", output: "skill instructions"},
		"empty output":    {command: "sed -n '1,240p' '" + instruction + "'", output: " \n\t"},
		"empty codex envelope": {
			command: "sed -n '1,240p' '" + instruction + "'",
			output:  "Process exited with code 0\nFinal output:\n",
		},
		"whitespace codex envelope": {
			command: "sed -n '1,240p' '" + instruction + "'",
			output:  "Process exited with code 0\nFinal output:\n \n\t",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			messages := []domain.Message{
				{Role: "tool", ToolName: "exec_command", ToolCallID: "read", Text: `{"cmd":` + quotedJSON(t, test.command) + `}`},
				{Role: "tool", ToolName: "exec_command", ToolCallID: "read", ToolResult: true, Text: test.output},
				{Role: "user", Text: "No, instead run the tests"},
			}
			cards := NewAnalyzer().Analyze(
				domain.Session{Source: domain.SourceCodex, ExternalID: name, Messages: messages},
				[]domain.Skill{skill},
			)
			if len(cards) != 0 {
				t.Fatalf("non-reading sed invocation attributed a skill: %#v", cards)
			}
		})
	}
}

func TestAnalyzeRejectsReadExecutablesAcceptedOnlyByBasename(t *testing.T) {
	const instruction = "/skills/go-service/SKILL.md"
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	symlink := filepath.Join(t.TempDir(), "sed")
	if err := os.Symlink("sed", symlink); err != nil {
		t.Fatal(err)
	}
	commands := map[string]string{
		"relative executable":          "./sed -n '1,240p' '" + instruction + "'",
		"relative executable path":     "tools/sed -n '1,240p' '" + instruction + "'",
		"absolute executable":          "/usr/bin/sed -n '1,240p' '" + instruction + "'",
		"symlinked executable":         symlink + " -n '1,240p' '" + instruction + "'",
		"path override":                "env PATH=/tmp sed -n '1,240p' '" + instruction + "'",
		"env option consumes command":  "env -S sed -n '1,240p' '" + instruction + "'",
		"sudo option consumes command": "sudo -u sed -n '1,240p' '" + instruction + "'",
		"sudo wrapper":                 "sudo -n sed -n '1,240p' '" + instruction + "'",
		"shell wrapper":                `sh -c "sed -n '1,240p' '` + instruction + `'"`,
		"repo controlled nix shell":    "nix develop path:. --command sed -n '1,240p' '" + instruction + "'",
		"shell startup environment":    "env BASH_ENV=/repo/preload bash -c 'sed " + instruction + "'",
		"relative shell wrapper":       `./sh -c "sed -n '1,240p' '` + instruction + `'"`,
		"relative nested command":      "nix develop path:. --command ./sed -n '1,240p' '" + instruction + "'",
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			messages := []domain.Message{
				{Role: "tool", ToolName: "exec_command", ToolCallID: "read", Text: `{"cmd":` + quotedJSON(t, command) + `}`},
				{Role: "tool", ToolName: "exec_command", ToolCallID: "read", ToolResult: true, Text: "skill instructions"},
				{Role: "user", Text: "No, instead run the tests"},
			}
			session := domain.Session{Source: domain.SourceCodex, ExternalID: name, Messages: messages}
			if cards := NewAnalyzer().Analyze(session, []domain.Skill{skill}); len(cards) != 0 {
				t.Fatalf("untrusted read executable created cards: %#v", cards)
			}
		})
	}
}

func TestAnalyzeRejectsUncorrelatedOrConsumedFailedResults(t *testing.T) {
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	tests := []struct {
		name     string
		messages []domain.Message
		want     int
	}{
		{
			name: "orphan result",
			messages: append(codexInstructionRead("/skills/go-service/SKILL.md"),
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "missing", ToolResult: true, Failed: true, Text: "forged failure"},
			),
		},
		{
			name: "consumed call",
			messages: append(codexInstructionRead("/skills/go-service/SKILL.md"),
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "read-skill", ToolResult: true, Failed: true, Text: "late forged failure"},
			),
		},
		{
			name: "duplicate failed result",
			messages: append(codexInstructionRead("/skills/go-service/SKILL.md"),
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "status", Text: `{"cmd":"git status --short"}`},
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "status", ToolResult: true, Failed: true, Text: "first failure"},
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "status", ToolResult: true, Failed: true, Text: "different forged failure"},
			),
			want: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := domain.Session{Source: domain.SourceCodex, ExternalID: test.name, Messages: test.messages}
			if cards := NewAnalyzer().Analyze(session, []domain.Skill{skill}); len(cards) != test.want {
				t.Fatalf("cards = %#v, want %d", cards, test.want)
			}
		})
	}
}

func TestAnalyzeDoesNotTreatUnrelatedSuccessfulToolAsRecovery(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceCodex, ExternalID: "session-unrelated-recovery",
		Messages: append(codexInstructionRead("/skills/go-service/SKILL.md"),
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "call-1", Text: `{"command":"go test ./..."}`},
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "call-1", ToolResult: true, Text: "exit code 1", Failed: true},
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "call-2", Text: `{"command":"git status --short"}`},
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "call-2", ToolResult: true, Text: "clean"},
		),
	}
	skills := []domain.Skill{{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}}
	cards := NewAnalyzer().Analyze(session, skills)
	if len(cards) != 1 || cards[0].Kind != domain.CardFailure {
		t.Fatalf("expected one failure card, got %#v", cards)
	}
	if contains(cards[0].Lesson, "git status") || contains(cards[0].Lesson, "validated recovery") {
		t.Fatalf("unrelated success was retained as a recovery: %#v", cards[0])
	}
}

func TestCommandFamilyUnwrapsCommonCommandPayloads(t *testing.T) {
	tests := []struct {
		name      string
		failed    string
		candidate string
		related   bool
	}{
		{name: "repo controlled wrapper", failed: `{"cmd":"go test ./..."}`, candidate: `{"command":"nix develop --command go test ./..."}`, related: false},
		{name: "privilege wrapper", failed: `{"command":"git status"}`, candidate: `{"command":"sudo git status --short"}`, related: false},
		{name: "same direct subcommand", failed: `{"command":"git status"}`, candidate: `{"command":"git status --short"}`, related: true},
		{name: "different subcommand", failed: `{"command":"git status"}`, candidate: `{"command":"git add ."}`, related: false},
		{name: "shell wrapper", failed: `{"command":"make verify"}`, candidate: `{"command":"bash -c 'make verify'"}`, related: false},
		{name: "shell startup environment", failed: `{"command":"go test ./..."}`, candidate: `{"command":"env BASH_ENV=/repo/preload bash -c 'go test ./...'"}`, related: false},
		{name: "safe environment changes proof", failed: `{"command":"go test ./..."}`, candidate: `{"command":"env CGO_ENABLED=0 go test ./..."}`, related: false},
		{name: "command wrapper", failed: `{"command":"go test ./..."}`, candidate: `{"command":"command -- go test ./..."}`, related: true},
		{name: "non-executing validation", failed: `{"command":"go test ./..."}`, candidate: `{"command":"go test -exec=true ./..."}`, related: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := relatedToolCalls(test.failed, test.candidate); got != test.related {
				t.Fatalf("relatedToolCalls() = %v, want %v", got, test.related)
			}
		})
	}
}

func TestAnalyzeRejectsAmbiguousAttribution(t *testing.T) {
	session := domain.Session{Source: domain.SourceClaude, ExternalID: "session-2", Messages: append(
		claudeInstructionRead("/skills/alpha/SKILL.md"),
		append(claudeInstructionReadWithID("/skills/beta/SKILL.md", "read-beta"),
			domain.Message{Role: "user", Text: "No, instead run the tests."},
		)...,
	)}
	skills := []domain.Skill{
		{ID: "a", Name: "alpha", RepositoryPath: "/skills/alpha", InstructionPath: "SKILL.md", Enabled: true},
		{ID: "b", Name: "beta", RepositoryPath: "/skills/beta", InstructionPath: "SKILL.md", Enabled: true},
	}
	if cards := NewAnalyzer().Analyze(session, skills); len(cards) != 0 {
		t.Fatalf("expected ambiguous attribution to be rejected, got %#v", cards)
	}
}

func TestAnalyzeRequiresSuccessfulCorrelatedInstructionEvidence(t *testing.T) {
	instruction := "/skills/go-service/SKILL.md"
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	tests := []struct {
		name     string
		messages []domain.Message
	}{
		{
			name: "user path mention",
			messages: []domain.Message{
				{Role: "user", Text: "No, instead change " + instruction},
			},
		},
		{
			name: "assistant load claim",
			messages: []domain.Message{
				{Role: "assistant", Text: "Loaded " + instruction},
				{Role: "user", Text: "No, instead run the tests"},
			},
		},
		{
			name: "uncorrelated result mention",
			messages: []domain.Message{
				{Role: "tool", ToolName: "Read", ToolCallID: "missing", ToolResult: true, Text: instruction},
				{Role: "user", Text: "No, instead run the tests"},
			},
		},
		{
			name: "failed read",
			messages: append([]domain.Message{
				{Role: "tool", ToolName: "Read", ToolCallID: "read", Text: `{"file_path":"` + instruction + `"}`},
				{Role: "tool", ToolName: "Read", ToolCallID: "read", ToolResult: true, Failed: true, Text: "permission denied"},
			}, domain.Message{Role: "user", Text: "No, instead run the tests"}),
		},
		{
			name: "read result contains enoent despite missing failure flag",
			messages: []domain.Message{
				{Role: "tool", ToolName: "Read", ToolCallID: "read", Text: `{"file_path":"` + instruction + `"}`},
				{Role: "tool", ToolName: "Read", ToolCallID: "read", ToolResult: true, Text: "ENOENT: no such file or directory"},
				{Role: "user", Text: "No, instead run the tests"},
			},
		},
		{
			name: "successful echo",
			messages: []domain.Message{
				{Role: "tool", ToolName: "exec_command", ToolCallID: "echo", Text: `{"cmd":"echo ` + instruction + `"}`},
				{Role: "tool", ToolName: "exec_command", ToolCallID: "echo", ToolResult: true, Text: instruction},
				{Role: "user", Text: "No, instead run the tests"},
			},
		},
		{
			name: "reader help does not read path",
			messages: []domain.Message{
				{Role: "tool", ToolName: "exec_command", ToolCallID: "help", Text: `{"cmd":"cat --help ` + instruction + `"}`},
				{Role: "tool", ToolName: "exec_command", ToolCallID: "help", ToolResult: true, Text: "usage"},
				{Role: "user", Text: "No, instead run the tests"},
			},
		},
		{
			name: "read path only as search option",
			messages: []domain.Message{
				{Role: "tool", ToolName: "exec_command", ToolCallID: "search", Text: `{"cmd":"rg pattern --glob ` + instruction + ` other.md"}`},
				{Role: "tool", ToolName: "exec_command", ToolCallID: "search", ToolResult: true, Text: "ok"},
				{Role: "user", Text: "No, instead run the tests"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := domain.Session{Source: domain.SourceCodex, ExternalID: test.name, Messages: test.messages}
			if cards := NewAnalyzer().Analyze(session, []domain.Skill{skill}); len(cards) != 0 {
				t.Fatalf("untrusted evidence created cards: %#v", cards)
			}
		})
	}
}

func TestAnalyzeRejectsForgedToolNamesForSkillAttribution(t *testing.T) {
	instruction := "/skills/go-service/SKILL.md"
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	tests := []struct {
		name     string
		source   domain.Source
		toolName string
		payload  string
	}{
		{name: "codex database read", source: domain.SourceCodex, toolName: "database_read", payload: `{"path":"` + instruction + `"}`},
		{name: "codex suffix read", source: domain.SourceCodex, toolName: "mcp__filesystem__read", payload: `{"path":"` + instruction + `"}`},
		{name: "codex suffix shell", source: domain.SourceCodex, toolName: "exec_command_suffix", payload: `{"cmd":"cat ` + instruction + `"}`},
		{name: "codex cross provider read", source: domain.SourceCodex, toolName: "Read", payload: `{"file_path":"` + instruction + `"}`},
		{name: "claude database read", source: domain.SourceClaude, toolName: "database_read", payload: `{"path":"` + instruction + `"}`},
		{name: "claude namespaced read", source: domain.SourceClaude, toolName: "tools__Read", payload: `{"file_path":"` + instruction + `"}`},
		{name: "claude suffix read", source: domain.SourceClaude, toolName: "Read_suffix", payload: `{"file_path":"` + instruction + `"}`},
		{name: "claude lowercase read", source: domain.SourceClaude, toolName: "read", payload: `{"file_path":"` + instruction + `"}`},
		{name: "claude suffix bash", source: domain.SourceClaude, toolName: "Bash_suffix", payload: `{"command":"cat ` + instruction + `"}`},
		{name: "claude cross provider shell", source: domain.SourceClaude, toolName: "exec_command", payload: `{"cmd":"cat ` + instruction + `"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := domain.Session{Source: test.source, ExternalID: test.name, Messages: []domain.Message{
				{Role: "tool", ToolName: test.toolName, ToolCallID: "forged", Text: test.payload},
				{Role: "tool", ToolName: test.toolName, ToolCallID: "forged", ToolResult: true, Text: "skill instructions"},
				{Role: "user", Text: "No, instead run the tests"},
			}}
			if cards := NewAnalyzer().Analyze(session, []domain.Skill{skill}); len(cards) != 0 {
				t.Fatalf("forged tool name attributed a skill: %#v", cards)
			}
		})
	}
}

func TestAnalyzeAttributesOnlyTheSkillWithTrustedReadEvidence(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceCodex, ExternalID: "trusted-read-wins",
		Messages: append(codexInstructionRead("/skills/actual/SKILL.md"),
			domain.Message{Role: "user", Text: "No, instead record this for target-skill and /skills/target/SKILL.md"},
		),
	}
	skills := []domain.Skill{
		{ID: "actual", Name: "actual-skill", RepositoryPath: "/skills/actual", InstructionPath: "SKILL.md", Enabled: true},
		{ID: "target", Name: "target-skill", RepositoryPath: "/skills/target", InstructionPath: "SKILL.md", Enabled: true},
	}
	cards := NewAnalyzer().Analyze(session, skills)
	if len(cards) != 1 || cards[0].SkillID != "actual" {
		t.Fatalf("cards = %#v, want only the actually read skill", cards)
	}
}

func TestAnalyzeAcceptsProviderNormalizedInstructionReads(t *testing.T) {
	instruction := "/skills/go-service/SKILL.md"
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	tests := []struct {
		name     string
		source   domain.Source
		messages []domain.Message
	}{
		{name: "codex shell read", source: domain.SourceCodex, messages: codexInstructionRead(instruction)},
		{name: "claude read tool", source: domain.SourceClaude, messages: claudeInstructionRead(instruction)},
		{
			name: "claude skill loader", source: domain.SourceClaude,
			messages: []domain.Message{
				{Role: "tool", ToolName: "Skill", ToolCallID: "skill-load", Text: `{"skill":"go-service"}`},
				{Role: "tool", ToolName: "Skill", ToolCallID: "skill-load", ToolResult: true, Text: "loaded"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			messages := make([]domain.Message, 0, len(test.messages)+1)
			messages = append(messages, test.messages...)
			messages = append(messages, domain.Message{Role: "user", Text: "No, instead run the tests"})
			session := domain.Session{Source: test.source, ExternalID: test.name, Messages: messages}
			cards := NewAnalyzer().Analyze(session, []domain.Skill{skill})
			if len(cards) != 1 || cards[0].SkillID != skill.ID || cards[0].Kind != domain.CardCorrection {
				t.Fatalf("cards = %#v, want one attributed correction", cards)
			}
		})
	}
}

func TestAnalyzeAttributesMatchingInstalledSkillCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repository := t.TempDir()
	source := filepath.Join(repository, "SKILL.md")
	contents := []byte("---\nname: go-service\ndescription: Go workflow\n---\n")
	if err := os.WriteFile(source, contents, 0o600); err != nil {
		t.Fatalf("write source skill: %v", err)
	}
	storeSkill := filepath.Join(t.TempDir(), "go-service")
	if err := os.MkdirAll(storeSkill, 0o700); err != nil {
		t.Fatalf("create installed skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storeSkill, "SKILL.md"), contents, 0o600); err != nil {
		t.Fatalf("write installed skill: %v", err)
	}
	installed := filepath.Join(home, ".agents", "skills", "go-service")
	if err := os.MkdirAll(filepath.Dir(installed), 0o700); err != nil {
		t.Fatalf("create installation root: %v", err)
	}
	if err := os.Symlink(storeSkill, installed); err != nil {
		t.Fatalf("link Nix-style installed skill: %v", err)
	}

	instruction := filepath.Join(installed, "SKILL.md")
	messages := append(codexInstructionRead(instruction), domain.Message{Role: "user", Text: "Non, il faut lancer les tests."})
	skill := domain.Skill{
		ID: "skill-1", Name: "Go Service", RepositoryPath: repository,
		InstructionPath: "SKILL.md", Enabled: true,
	}
	cards := NewAnalyzer().Analyze(
		domain.Session{Source: domain.SourceCodex, ExternalID: "installed-copy", Messages: messages},
		[]domain.Skill{skill},
	)
	if len(cards) != 1 || cards[0].SkillID != skill.ID {
		t.Fatalf("cards = %#v, want installed copy attributed to source skill", cards)
	}

	if err := os.WriteFile(filepath.Join(storeSkill, "SKILL.md"), []byte("different instructions"), 0o600); err != nil {
		t.Fatalf("replace installed skill: %v", err)
	}
	if cards := NewAnalyzer().Analyze(
		domain.Session{Source: domain.SourceCodex, ExternalID: "diverged-copy", Messages: messages},
		[]domain.Skill{skill},
	); len(cards) != 0 {
		t.Fatalf("diverged installed copy was attributed: %#v", cards)
	}
}

func TestFailureFingerprintBindsNormalizedRecovery(t *testing.T) {
	makeCard := func(sessionID, recovery string) domain.LearningCard {
		session := domain.Session{
			Source: domain.SourceCodex, ExternalID: sessionID,
			Messages: append(codexInstructionRead("/skills/go-service/SKILL.md"),
				domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "failed", Text: `{"command":"go test ./..."}`},
				domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "failed", ToolResult: true, Failed: true, Text: "exit code 1"},
				domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "recovery", Text: recovery},
				domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "recovery", ToolResult: true, Text: "ok"},
			),
		}
		skill := domain.Skill{
			ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
			InstructionPath: "SKILL.md", Enabled: true,
		}
		cards := NewAnalyzer().Analyze(session, []domain.Skill{skill})
		for _, card := range cards {
			if card.Kind == domain.CardFailure {
				return card
			}
		}
		t.Fatalf("no failure card in %#v", cards)
		return domain.LearningCard{}
	}

	first := makeCard("a", `{"command":"go test ./..."}`)
	concordant := makeCard("b", `{"command":"go   test ./..."}`)
	disagreeing := makeCard("c", `{"command":"go test -run Hostile ./..."}`)
	if first.Fingerprint != concordant.Fingerprint {
		t.Fatalf("normalized concordant recoveries split: %q != %q", first.Fingerprint, concordant.Fingerprint)
	}
	if first.Fingerprint == disagreeing.Fingerprint {
		t.Fatalf("disagreeing recovery inherited fingerprint %q", first.Fingerprint)
	}
}

func codexInstructionRead(path string) []domain.Message {
	return []domain.Message{
		{Role: "tool", ToolName: "exec_command", ToolCallID: "read-skill", Text: `{"cmd":"sed -n '1,240p' '` + path + `'"}`},
		{Role: "tool", ToolName: "exec_command", ToolCallID: "read-skill", ToolResult: true, Text: "skill instructions"},
	}
}

func claudeInstructionRead(path string) []domain.Message {
	return claudeInstructionReadWithID(path, "read-skill")
}

func claudeInstructionReadWithID(path, id string) []domain.Message {
	return []domain.Message{
		{Role: "tool", ToolName: "Read", ToolCallID: id, Text: `{"file_path":"` + path + `"}`},
		{Role: "tool", ToolName: "Read", ToolCallID: id, ToolResult: true, Text: "skill instructions"},
	}
}

func contains(value, substring string) bool {
	return strings.Contains(value, substring)
}

func quotedJSON(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func validationOutput(value string) string {
	if value == "" {
		return "ok"
	}
	return value
}
