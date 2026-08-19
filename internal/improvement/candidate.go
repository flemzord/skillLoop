package improvement

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/flemzord/skillloop/internal/domain"
)

const (
	maxDiffBytes = 4 * 1024
	maxDiffLines = 30
)

var (
	fingerprintPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	managedMarker      = regexp.MustCompile(`^<!-- skillloop:(begin|end) ([A-Za-z0-9][A-Za-z0-9._:-]{0,127}) -->$`)
	sensitiveChange    = regexp.MustCompile(`(?i)\b(security|permissions?|secrets?|credentials?|passwords?|passphrases?|api[ _-]?keys?|private[ _-]?keys?|access[ _-]?tokens?|auth(?:entication|orization)?|oauth|sudo|chmod|sandbox|firewalls?|privileges?|roles?|acl|iam|ssh|tls|certificates?|encryption|cryptograph(?:y|ic))\b`)
	promptInjection    = regexp.MustCompile(`(?i)\b(ignore (all |any )?(previous|prior)|system prompt|developer message|jailbreak|bypass (the )?(instructions?|guardrails?|safety)|override (the )?instructions?|disable (the )?(guardrails?|safety)|exfiltrate|prompt injection)\b`)
	secretPatterns     = []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`),
		regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{20,}\b`),
		regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
		regexp.MustCompile(`(?i)\b(password|passwd|secret|access_token|api_key)\s*[:=]\s*[^\s$<{][^\s]{7,}`),
	}
)

// Prepare creates and commits a candidate in an isolated worktree. The source
// checkout is never staged, cleaned, reset, or otherwise modified.
func (service Service) Prepare(ctx context.Context, skill domain.Skill, cluster domain.Cluster) (Candidate, error) {
	if !skill.Enabled {
		return Candidate{}, errors.New("skill is not enabled and owned")
	}
	if skill.ID == "" || cluster.ID == "" || cluster.SkillID != skill.ID {
		return Candidate{}, errors.New("cluster does not belong to skill")
	}
	if cluster.SessionCount < 2 {
		return Candidate{}, errors.New("candidate requires at least two concordant sessions")
	}
	if cluster.Status != domain.ClusterOpen {
		return Candidate{}, errors.New("candidate requires an open cluster")
	}
	if strings.TrimSpace(cluster.Fingerprint) == "" {
		return Candidate{}, errors.New("cluster fingerprint is required")
	}
	fingerprint := MarkerFingerprint(cluster.Fingerprint)
	lesson := normalizeLesson(cluster.Lesson)
	if lesson == "" {
		return Candidate{}, errors.New("cluster lesson is required")
	}
	if containsSecret(lesson) {
		return Candidate{}, ErrUnsafeChange
	}
	requiresHumanApproval := sensitiveChange.MatchString(cluster.Summary+"\n"+lesson) ||
		promptInjection.MatchString(cluster.Summary+"\n"+lesson)

	repository, err := resolveRepository(ctx, skill.RepositoryPath)
	if err != nil {
		return Candidate{}, err
	}
	_, relative, err := resolveInstruction(repository, skill.InstructionPath)
	if err != nil {
		return Candidate{}, err
	}
	sourceHead, err := git(ctx, repository, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return Candidate{}, err
	}
	base := sourceHead
	current, currentErr := service.CurrentRelease(skill)
	if currentErr == nil {
		base = current.Commit
		if _, err := git(ctx, repository, "merge-base", "--is-ancestor", sourceHead, base); err != nil {
			return Candidate{}, fmt.Errorf("source HEAD diverged from current release: %w", ErrDrift)
		}
	} else if !errors.Is(currentErr, ErrNoRelease) {
		return Candidate{}, currentErr
	}
	if err := commitExists(ctx, repository, base); err != nil {
		return Candidate{}, err
	}
	if err := verifyTrackedInstruction(ctx, repository, base, relative); err != nil {
		return Candidate{}, err
	}

	suffix, err := randomSuffix()
	if err != nil {
		return Candidate{}, err
	}
	stateDir, err := filepath.Abs(service.StateDir)
	if err != nil || service.StateDir == "" {
		return Candidate{}, errors.New("state directory is required")
	}
	worktree := filepath.Join(stateDir, "worktrees", safeName(skill.ID), safeName(cluster.ID)+"-"+suffix)
	if err := os.MkdirAll(filepath.Dir(worktree), 0o700); err != nil {
		return Candidate{}, fmt.Errorf("create worktree parent: %w", err)
	}
	branch := "skillloop/" + safeName(skill.ID) + "/" + safeName(cluster.ID) + "-" + suffix
	if _, err := git(ctx, repository, "worktree", "add", "-b", branch, worktree, base); err != nil {
		return Candidate{}, err
	}
	keepWorktree := false
	defer func() {
		if keepWorktree {
			return
		}
		_, _ = git(context.Background(), repository, "worktree", "remove", "--force", worktree)
		_, _ = git(context.Background(), repository, "branch", "-D", branch)
	}()

	instruction := filepath.Join(worktree, filepath.FromSlash(relative))
	currentContents, err := os.ReadFile(instruction)
	if err != nil {
		return Candidate{}, fmt.Errorf("read candidate SKILL.md: %w", err)
	}
	updated, err := applyManagedBlock(currentContents, fingerprint, lesson)
	if err != nil {
		return Candidate{}, err
	}
	if string(updated) == string(currentContents) {
		return Candidate{}, errors.New("candidate produces no change")
	}
	if containsSecret(string(updated)) {
		return Candidate{}, ErrUnsafeChange
	}
	info, err := os.Stat(instruction)
	if err != nil {
		return Candidate{}, fmt.Errorf("stat candidate SKILL.md: %w", err)
	}
	if err := os.WriteFile(instruction, updated, info.Mode().Perm()); err != nil {
		return Candidate{}, fmt.Errorf("write candidate SKILL.md: %w", err)
	}

	changed, err := git(ctx, worktree, "diff", "--name-only")
	if err != nil {
		return Candidate{}, err
	}
	if changed != relative {
		return Candidate{}, fmt.Errorf("candidate changed %q instead of only %q: %w", changed, relative, ErrUnsafePath)
	}
	diff, err := git(ctx, worktree, "diff", "--no-ext-diff", "--", relative)
	if err != nil {
		return Candidate{}, err
	}
	if err := validateDiff(diff); err != nil {
		return Candidate{}, err
	}
	if containsSecret(diff) {
		return Candidate{}, ErrUnsafeChange
	}
	reapplied, err := applyManagedBlock(updated, fingerprint, lesson)
	if err != nil || string(reapplied) != string(updated) {
		return Candidate{}, errors.New("candidate patch is not idempotent")
	}

	if _, err := git(ctx, worktree, "add", "--", relative); err != nil {
		return Candidate{}, err
	}
	message := "feat(skill): improve " + safeName(skill.Name)
	if skill.Name == "" {
		message = "feat(skill): improve managed guidance"
	}
	if _, err := git(ctx, worktree,
		"-c", "user.name=SkillLoop",
		"-c", "user.email=skillloop@localhost",
		"commit", "-m", message,
	); err != nil {
		return Candidate{}, err
	}
	candidateCommit, err := git(ctx, worktree, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return Candidate{}, err
	}
	parent, err := git(ctx, repository, "rev-parse", candidateCommit+"^")
	if err != nil || parent != base {
		return Candidate{}, fmt.Errorf("candidate parent is not exact baseline: %w", ErrDrift)
	}
	committedDiff, err := git(ctx, repository, "diff", "--no-ext-diff", base, candidateCommit, "--", relative)
	if err != nil {
		return Candidate{}, err
	}
	if committedDiff != diff {
		return Candidate{}, fmt.Errorf("committed diff changed during preparation: %w", ErrDrift)
	}

	keepWorktree = true
	return Candidate{
		SkillID:               skill.ID,
		ClusterID:             cluster.ID,
		Fingerprint:           fingerprint,
		Lesson:                lesson,
		RepositoryPath:        repository,
		InstructionPath:       relative,
		WorktreePath:          worktree,
		Branch:                branch,
		BaseCommit:            base,
		CandidateCommit:       candidateCommit,
		Diff:                  diff,
		RequiresHumanApproval: requiresHumanApproval,
		CreatedAt:             time.Now().UTC(),
	}, nil
}

// MarkerFingerprint returns a stable value safe to embed in an HTML comment.
// Already-safe fingerprints remain human-readable; arbitrary normalized
// fingerprints use a deterministic SHA-256 prefix.
func MarkerFingerprint(raw string) string {
	raw = strings.TrimSpace(raw)
	if fingerprintPattern.MatchString(raw) {
		return raw
	}
	digest := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("fp-%x", digest[:16])
}

func applyManagedBlock(contents []byte, fingerprint, lesson string) ([]byte, error) {
	if !utf8.Valid(contents) || bytesContainNUL(contents) {
		return nil, errors.New("SKILL.md must be UTF-8 text")
	}
	if err := validateStructure(string(contents)); err != nil {
		return nil, err
	}
	begin := "<!-- skillloop:begin " + fingerprint + " -->"
	end := "<!-- skillloop:end " + fingerprint + " -->"
	if strings.Contains(lesson, "<!-- skillloop:") {
		return nil, errors.New("lesson contains a reserved SkillLoop marker")
	}
	block := begin + "\n### Learned guidance\n\n" + lesson + "\n" + end
	text := string(contents)
	beginIndex := strings.Index(text, begin)
	endIndex := strings.Index(text, end)
	switch {
	case beginIndex < 0 && endIndex < 0:
		text = strings.TrimRight(text, "\n") + "\n\n" + block + "\n"
	case beginIndex < 0 || endIndex < beginIndex:
		return nil, errors.New("malformed managed block")
	default:
		endIndex += len(end)
		if strings.Contains(text[beginIndex+len(begin):endIndex-len(end)], begin) || strings.Contains(text[endIndex:], end) {
			return nil, errors.New("duplicate managed block")
		}
		text = text[:beginIndex] + block + text[endIndex:]
	}
	if err := validateStructure(text); err != nil {
		return nil, err
	}
	return []byte(text), nil
}

func validateStructure(contents string) error {
	if strings.TrimSpace(contents) == "" {
		return errors.New("SKILL.md is empty")
	}
	if strings.HasPrefix(contents, "---\n") {
		if end := strings.Index(contents[4:], "\n---\n"); end < 0 {
			return errors.New("SKILL.md has malformed front matter")
		}
	}
	activeFingerprint := ""
	for line := range strings.SplitSeq(contents, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "<!-- skillloop:begin ") && !strings.Contains(trimmed, "<!-- skillloop:end ") {
			continue
		}
		match := managedMarker.FindStringSubmatch(trimmed)
		if len(match) != 3 {
			return errors.New("SKILL.md has a malformed SkillLoop marker")
		}
		switch match[1] {
		case "begin":
			if activeFingerprint != "" {
				return errors.New("SKILL.md has nested SkillLoop blocks")
			}
			activeFingerprint = match[2]
		case "end":
			if activeFingerprint != match[2] {
				return errors.New("SKILL.md has mismatched SkillLoop markers")
			}
			activeFingerprint = ""
		}
	}
	if activeFingerprint != "" {
		return errors.New("SKILL.md has an unclosed SkillLoop block")
	}
	if strings.Count(contents, "```")%2 != 0 {
		return errors.New("SKILL.md has an unbalanced fenced code block")
	}
	return nil
}

func normalizeLesson(lesson string) string {
	lesson = strings.ReplaceAll(lesson, "\r\n", "\n")
	lesson = strings.ReplaceAll(lesson, "\r", "\n")
	return strings.TrimSpace(lesson)
}

func validateDiff(diff string) error {
	lines, err := lineCount(diff)
	if err != nil {
		return err
	}
	if lines > maxDiffLines || len([]byte(diff)) > maxDiffBytes {
		return fmt.Errorf("diff has %d changed lines and %d bytes: %w", lines, len([]byte(diff)), ErrDiffLimit)
	}
	return nil
}

func containsSecret(contents string) bool {
	for _, pattern := range secretPatterns {
		if pattern.MatchString(contents) {
			return true
		}
	}
	return false
}

func bytesContainNUL(contents []byte) bool {
	return slices.Contains(contents, byte(0))
}
