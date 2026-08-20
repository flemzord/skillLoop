package improvement

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/sanitize"
)

// Restore reconstructs a candidate from its immutable proposal metadata. The
// cluster is used only for ownership; its mutable aggregate text must never
// change the safety decision for an already-created candidate.
func (service Service) Restore(ctx context.Context, skill domain.Skill, cluster domain.Cluster, proposal domain.Proposal) (Candidate, error) {
	if proposal.SkillID != skill.ID || proposal.ClusterID != cluster.ID || cluster.SkillID != skill.ID {
		return Candidate{}, fmt.Errorf("proposal ownership changed: %w", ErrDrift)
	}
	legacyMetadata := proposal.Fingerprint == "" && proposal.Lesson == "" && proposal.CardKind == ""
	if legacyMetadata && !proposal.RequiresHumanApproval {
		return Candidate{}, fmt.Errorf("legacy proposal safety default was weakened: %w", ErrDrift)
	}
	if !legacyMetadata && proposal.CardKind != cluster.Kind {
		return Candidate{}, fmt.Errorf("proposal learning-card kind changed: %w", ErrDrift)
	}
	repository, err := resolveRepository(ctx, skill.RepositoryPath)
	if err != nil {
		return Candidate{}, err
	}
	if !samePath(repository, proposal.RepositoryPath) {
		return Candidate{}, fmt.Errorf("proposal repository changed: %w", ErrDrift)
	}
	_, instruction, err := resolveInstruction(repository, skill.InstructionPath)
	if err != nil {
		return Candidate{}, err
	}
	if err := commitExists(ctx, repository, proposal.BaseCommit); err != nil {
		return Candidate{}, err
	}
	if err := commitExists(ctx, repository, proposal.CandidateCommit); err != nil {
		return Candidate{}, err
	}
	if !legacyMetadata && (proposal.Fingerprint == "" || !fingerprintPattern.MatchString(proposal.Fingerprint) ||
		normalizeLesson(proposal.Lesson) != proposal.Lesson || proposal.Lesson == "") {
		return Candidate{}, fmt.Errorf("proposal candidate metadata is incomplete: %w", ErrDrift)
	}
	if err := verifyTrackedInstruction(ctx, repository, proposal.BaseCommit, instruction); err != nil {
		return Candidate{}, err
	}
	if err := verifyTrackedInstruction(ctx, repository, proposal.CandidateCommit, instruction); err != nil {
		return Candidate{}, err
	}
	parent, err := git(ctx, repository, "rev-parse", proposal.CandidateCommit+"^")
	if err != nil || parent != proposal.BaseCommit {
		return Candidate{}, fmt.Errorf("candidate parent is not exact baseline: %w", ErrDrift)
	}
	baselineContents, err := gitBytesLimit(ctx, repository, maxSkillFileBytes, "show", proposal.BaseCommit+":"+instruction)
	if err != nil {
		return Candidate{}, err
	}
	candidateContents, err := gitBytesLimit(ctx, repository, maxSkillFileBytes, "show", proposal.CandidateCommit+":"+instruction)
	if err != nil {
		return Candidate{}, err
	}
	fingerprint := proposal.Fingerprint
	lesson := proposal.Lesson
	cardKind := proposal.CardKind
	requiresHumanApproval := proposal.RequiresHumanApproval
	if legacyMetadata {
		fingerprint, lesson, err = recoverLegacyManagedBlock(baselineContents, candidateContents)
		if err != nil {
			return Candidate{}, err
		}
		cardKind = cluster.Kind
		if _, err := classifyCandidate(cardKind, "", lesson); err != nil {
			return Candidate{}, err
		}
		// Schema v1 had no durable safety metadata. A reconstructed candidate
		// is therefore always human-only, even when its cluster currently has a
		// validation kind that would otherwise be eligible for autopilot.
		requiresHumanApproval = true
	} else {
		requiresHuman, classifyErr := classifyCandidate(cardKind, "", lesson)
		if classifyErr != nil {
			return Candidate{}, classifyErr
		}
		if requiresHuman && !requiresHumanApproval {
			return Candidate{}, fmt.Errorf("proposal safety classification was weakened: %w", ErrDrift)
		}
	}
	if sanitize.ContainsSecret(lesson) {
		return Candidate{}, ErrUnsafeChange
	}
	expectedContents, err := applyManagedBlock(baselineContents, fingerprint, lesson)
	if err != nil || !bytes.Equal(expectedContents, candidateContents) {
		return Candidate{}, fmt.Errorf("durable proposal metadata does not reproduce candidate: %w", ErrDrift)
	}
	if sanitize.ContainsSecret(string(candidateContents)) {
		return Candidate{}, ErrUnsafeChange
	}
	changed, err := git(ctx, repository, "diff", "--no-ext-diff", "--no-textconv", "--name-only", proposal.BaseCommit, proposal.CandidateCommit)
	if err != nil {
		return Candidate{}, err
	}
	if changed != instruction {
		return Candidate{}, fmt.Errorf("candidate changed %q instead of only %q: %w", changed, instruction, ErrUnsafePath)
	}
	diff, err := git(ctx, repository, "diff", "--no-ext-diff", "--no-textconv", proposal.BaseCommit, proposal.CandidateCommit, "--", instruction)
	if err != nil {
		return Candidate{}, err
	}
	if err := validateDiff(diff); err != nil {
		return Candidate{}, err
	}
	if sanitize.ContainsSecret(diff) {
		return Candidate{}, ErrUnsafeChange
	}
	return Candidate{
		SkillID:               skill.ID,
		ClusterID:             cluster.ID,
		Fingerprint:           fingerprint,
		Lesson:                lesson,
		CardKind:              cardKind,
		RepositoryPath:        repository,
		InstructionPath:       instruction,
		WorktreePath:          proposal.WorktreePath,
		Branch:                proposal.Branch,
		BaseCommit:            proposal.BaseCommit,
		CandidateCommit:       proposal.CandidateCommit,
		Diff:                  diff,
		RequiresHumanApproval: requiresHumanApproval,
		CreatedAt:             proposal.CreatedAt,
	}, nil
}

// recoverLegacyManagedBlock extracts candidate metadata only when exactly one
// canonical managed block can be applied to the baseline to reproduce the
// complete candidate contents. It never consults mutable cluster text.
func recoverLegacyManagedBlock(baselineContents, candidateContents []byte) (string, string, error) {
	if err := validateStructure(string(candidateContents)); err != nil {
		return "", "", fmt.Errorf("legacy candidate structure is invalid: %w", ErrDrift)
	}

	type metadata struct {
		fingerprint string
		lesson      string
	}
	var matches []metadata
	lines := strings.Split(string(candidateContents), "\n")
	for index, line := range lines {
		begin := managedMarker.FindStringSubmatch(line)
		if len(begin) != 3 || begin[1] != "begin" || index+3 >= len(lines) ||
			lines[index+1] != "### Learned guidance" || lines[index+2] != "" {
			continue
		}
		fingerprint := begin[2]
		endLine := "<!-- skillloop:end " + fingerprint + " -->"
		for end := index + 3; end < len(lines); end++ {
			if lines[end] != endLine {
				continue
			}
			lesson := strings.Join(lines[index+3:end], "\n")
			if lesson == "" || normalizeLesson(lesson) != lesson {
				break
			}
			reapplied, err := applyManagedBlock(baselineContents, fingerprint, lesson)
			if err == nil && bytes.Equal(reapplied, candidateContents) {
				matches = append(matches, metadata{fingerprint: fingerprint, lesson: lesson})
			}
			break
		}
	}
	if len(matches) != 1 {
		return "", "", fmt.Errorf("legacy candidate has %d uniquely reproducible managed blocks: %w", len(matches), ErrDrift)
	}
	return matches[0].fingerprint, matches[0].lesson, nil
}
