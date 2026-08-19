package improvement

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/flemzord/skillloop/internal/domain"
)

var releaseIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Promote materializes immutable snapshots for the exact approved revisions,
// then atomically switches the skill's current symlink to the candidate.
func (service Service) Promote(ctx context.Context, skill domain.Skill, candidate Candidate, evaluation Evaluation, approval Approval) (Promotion, error) {
	if !skill.Enabled || skill.ID == "" || skill.ID != candidate.SkillID {
		return Promotion{}, errors.New("candidate does not belong to an enabled owned skill")
	}
	if approval.BaseCommit != candidate.BaseCommit || approval.CandidateCommit != candidate.CandidateCommit {
		return Promotion{}, fmt.Errorf("approval revisions do not match candidate: %w", ErrDrift)
	}
	if evaluation.SkillID != candidate.SkillID || evaluation.ClusterID != candidate.ClusterID ||
		evaluation.BaseCommit != candidate.BaseCommit || evaluation.CandidateCommit != candidate.CandidateCommit {
		return Promotion{}, fmt.Errorf("evaluation revisions do not match candidate: %w", ErrDrift)
	}
	repository, err := resolveRepository(ctx, skill.RepositoryPath)
	if err != nil {
		return Promotion{}, err
	}
	if !samePath(repository, candidate.RepositoryPath) {
		return Promotion{}, fmt.Errorf("candidate repository changed: %w", ErrDrift)
	}
	_, relative, err := resolveInstruction(repository, skill.InstructionPath)
	if err != nil {
		return Promotion{}, err
	}
	if relative != candidate.InstructionPath {
		return Promotion{}, fmt.Errorf("candidate instruction path changed: %w", ErrDrift)
	}
	head, err := git(ctx, repository, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return Promotion{}, err
	}
	if _, err := git(ctx, repository, "merge-base", "--is-ancestor", head, candidate.BaseCommit); err != nil {
		return Promotion{}, fmt.Errorf("repository HEAD diverged from baseline: %w", ErrDrift)
	}
	if err := commitExists(ctx, repository, candidate.CandidateCommit); err != nil {
		return Promotion{}, err
	}
	parent, err := git(ctx, repository, "rev-parse", candidate.CandidateCommit+"^")
	if err != nil || parent != candidate.BaseCommit {
		return Promotion{}, fmt.Errorf("candidate no longer descends directly from baseline: %w", ErrDrift)
	}
	diff, err := git(ctx, repository, "diff", "--no-ext-diff", candidate.BaseCommit, candidate.CandidateCommit, "--", relative)
	if err != nil || diff != candidate.Diff {
		return Promotion{}, fmt.Errorf("candidate diff moved: %w", ErrDrift)
	}

	current, currentErr := service.CurrentRelease(skill)
	if currentErr != nil && !errors.Is(currentErr, ErrNoRelease) {
		return Promotion{}, currentErr
	}
	previousCommit := candidate.BaseCommit
	if currentErr == nil && current.Commit == candidate.CandidateCommit {
		root, rootErr := service.skillReleaseRoot(skill.ID)
		if rootErr != nil {
			return Promotion{}, rootErr
		}
		previous, _, previousErr := readReleaseLink(root, "previous")
		if previousErr != nil || previous != candidate.BaseCommit {
			return Promotion{}, fmt.Errorf("already promoted release has mismatched previous revision: %w", ErrDrift)
		}
		return Promotion{
			SkillID: skill.ID, CurrentCommit: candidate.CandidateCommit,
			PreviousCommit: candidate.BaseCommit, ReleasePath: current.Path,
			CurrentLink: filepath.Join(root, "current"), PromotedAt: time.Now().UTC(),
		}, nil
	}
	if err := ValidatePromotionProof(evaluation); err != nil {
		return Promotion{}, err
	}
	if !evaluation.Passed {
		return Promotion{}, ErrEvaluationFailed
	}
	if currentErr == nil {
		branch, branchErr := git(ctx, repository, "rev-parse", "refs/heads/"+candidate.Branch)
		if branchErr != nil || branch != candidate.CandidateCommit {
			return Promotion{}, fmt.Errorf("candidate branch moved: %w", ErrDrift)
		}
		if current.Commit != candidate.BaseCommit {
			return Promotion{}, fmt.Errorf("current release differs from candidate baseline: %w", ErrDrift)
		}
		previousCommit = current.Commit
	} else {
		branch, branchErr := git(ctx, repository, "rev-parse", "refs/heads/"+candidate.Branch)
		if branchErr != nil || branch != candidate.CandidateCommit {
			return Promotion{}, fmt.Errorf("candidate branch moved: %w", ErrDrift)
		}
	}
	if _, err := service.materializeRelease(ctx, repository, skill, candidate.BaseCommit, relative); err != nil {
		return Promotion{}, err
	}
	candidateRelease, err := service.materializeRelease(ctx, repository, skill, candidate.CandidateCommit, relative)
	if err != nil {
		return Promotion{}, err
	}
	root, err := service.skillReleaseRoot(skill.ID)
	if err != nil {
		return Promotion{}, err
	}
	if err := pinRelease(ctx, repository, skill.ID, candidate.BaseCommit); err != nil {
		return Promotion{}, err
	}
	if err := pinRelease(ctx, repository, skill.ID, candidate.CandidateCommit); err != nil {
		return Promotion{}, err
	}
	if err := atomicSymlink(root, "previous", previousCommit); err != nil {
		return Promotion{}, err
	}
	if err := atomicSymlink(root, "current", candidate.CandidateCommit); err != nil {
		return Promotion{}, err
	}
	return Promotion{
		SkillID:        skill.ID,
		CurrentCommit:  candidate.CandidateCommit,
		PreviousCommit: previousCommit,
		ReleasePath:    candidateRelease,
		CurrentLink:    filepath.Join(root, "current"),
		PromotedAt:     time.Now().UTC(),
	}, nil
}

func pinRelease(ctx context.Context, repository, skillID, revision string) error {
	ref := "refs/skillloop/releases/" + safeName(skillID) + "/" + revision
	if _, err := git(ctx, repository, "update-ref", ref, revision); err != nil {
		return fmt.Errorf("pin release commit: %w", err)
	}
	return nil
}

// CurrentRelease returns the immutable snapshot selected by current.
func (service Service) CurrentRelease(skill domain.Skill) (Release, error) {
	root, err := service.skillReleaseRoot(skill.ID)
	if err != nil {
		return Release{}, err
	}
	commit, path, err := readReleaseLink(root, "current")
	if err != nil {
		if os.IsNotExist(err) {
			return Release{}, ErrNoRelease
		}
		return Release{}, err
	}
	return Release{SkillID: skill.ID, Commit: commit, Path: path}, nil
}

// Rollback atomically switches current to the previously promoted immutable
// snapshot. The old current becomes previous, allowing an immediate undo.
func (service Service) Rollback(_ context.Context, skill domain.Skill) (Promotion, error) {
	root, err := service.skillReleaseRoot(skill.ID)
	if err != nil {
		return Promotion{}, err
	}
	currentCommit, _, err := readReleaseLink(root, "current")
	if err != nil {
		if os.IsNotExist(err) {
			return Promotion{}, ErrNoRelease
		}
		return Promotion{}, err
	}
	previousCommit, previousPath, err := readReleaseLink(root, "previous")
	if err != nil {
		if os.IsNotExist(err) {
			return Promotion{}, ErrNoRelease
		}
		return Promotion{}, err
	}
	if previousCommit == currentCommit {
		return Promotion{}, errors.New("current and previous releases are identical")
	}
	if err := atomicSymlink(root, "current", previousCommit); err != nil {
		return Promotion{}, err
	}
	if err := atomicSymlink(root, "previous", currentCommit); err != nil {
		return Promotion{}, err
	}
	return Promotion{
		SkillID:        skill.ID,
		CurrentCommit:  previousCommit,
		PreviousCommit: currentCommit,
		ReleasePath:    previousPath,
		CurrentLink:    filepath.Join(root, "current"),
		RolledBack:     true,
		PromotedAt:     time.Now().UTC(),
	}, nil
}

func (service Service) skillReleaseRoot(skillID string) (string, error) {
	if !releaseIdentifier.MatchString(skillID) {
		return "", fmt.Errorf("invalid skill ID %q: %w", skillID, ErrUnsafePath)
	}
	if service.StateDir == "" {
		return "", errors.New("state directory is required")
	}
	stateDir, err := filepath.Abs(service.StateDir)
	if err != nil {
		return "", fmt.Errorf("resolve state directory: %w", err)
	}
	return filepath.Join(stateDir, "releases", skillID), nil
}

func (service Service) materializeRelease(ctx context.Context, repository string, skill domain.Skill, revision, instruction string) (string, error) {
	if err := commitExists(ctx, repository, revision); err != nil {
		return "", err
	}
	root, err := service.skillReleaseRoot(skill.ID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create releases directory: %w", err)
	}
	target := filepath.Join(root, revision)
	if info, statErr := os.Lstat(target); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("release path is not an immutable directory: %w", ErrUnsafePath)
		}
		if err := verifyRelease(ctx, repository, target, revision, instruction); err != nil {
			return "", err
		}
		if err := verifyImmutableDirectory(target); err != nil {
			return "", err
		}
		return target, nil
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("inspect release: %w", statErr)
	}

	skillDirectory := filepath.ToSlash(filepath.Dir(instruction))
	archiveArgs := []string{"archive", "--format=tar", revision}
	stripPrefix := ""
	if skillDirectory != "." {
		archiveArgs = append(archiveArgs, "--", skillDirectory)
		stripPrefix = strings.TrimSuffix(skillDirectory, "/") + "/"
	}
	archive, err := gitBytes(ctx, repository, archiveArgs...)
	if err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(root, ".release-")
	if err != nil {
		return "", fmt.Errorf("create release staging directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := extractArchive(archive, staging, stripPrefix); err != nil {
		return "", err
	}
	if err := verifyRelease(ctx, repository, staging, revision, instruction); err != nil {
		return "", err
	}
	if err := makeReadOnly(staging); err != nil {
		return "", err
	}
	if err := os.Rename(staging, target); err != nil {
		if info, statErr := os.Lstat(target); statErr == nil && info.IsDir() {
			if verifyErr := verifyRelease(ctx, repository, target, revision, instruction); verifyErr == nil {
				return target, nil
			}
		}
		return "", fmt.Errorf("publish immutable release: %w", err)
	}
	cleanup = false
	return target, nil
}

func extractArchive(archive []byte, destination, stripPrefix string) error {
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read git archive: %w", err)
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			// Git may emit a global PAX header for archive metadata. It has no
			// filesystem representation and is safe to ignore.
			continue
		}
		if stripPrefix != "" {
			if header.Typeflag == tar.TypeDir && strings.HasPrefix(stripPrefix, strings.TrimSuffix(header.Name, "/")+"/") {
				continue
			}
			if strings.TrimSuffix(header.Name, "/") == strings.TrimSuffix(stripPrefix, "/") {
				continue
			}
			if !strings.HasPrefix(header.Name, stripPrefix) {
				return fmt.Errorf("archive path %q is outside owned skill: %w", header.Name, ErrUnsafePath)
			}
			header.Name = strings.TrimPrefix(header.Name, stripPrefix)
		}
		name := filepath.FromSlash(header.Name)
		clean := filepath.Clean(name)
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive path %q escapes release: %w", header.Name, ErrUnsafePath)
		}
		target := filepath.Join(destination, clean)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("create release directory: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return fmt.Errorf("create release parent: %w", err)
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return fmt.Errorf("create release file: %w", err)
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract release file: %w", copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close release file: %w", closeErr)
			}
			mode := os.FileMode(header.Mode).Perm() &^ 0o222
			if err := os.Chmod(target, mode); err != nil {
				return fmt.Errorf("set release file mode: %w", err)
			}
		default:
			return fmt.Errorf("archive contains link or special file %q: %w", header.Name, ErrUnsafePath)
		}
	}
}

func verifyRelease(ctx context.Context, repository, release, revision, instruction string) error {
	expected, err := gitBytes(ctx, repository, "show", revision+":"+instruction)
	if err != nil {
		return err
	}
	actualPath := filepath.Join(release, filepath.Base(filepath.FromSlash(instruction)))
	info, err := os.Lstat(actualPath)
	if err != nil {
		return fmt.Errorf("inspect released SKILL.md: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("released SKILL.md is not a regular file: %w", ErrUnsafePath)
	}
	actual, err := os.ReadFile(actualPath)
	if err != nil {
		return fmt.Errorf("read released SKILL.md: %w", err)
	}
	if !bytes.Equal(expected, actual) {
		return fmt.Errorf("release contents differ from commit: %w", ErrDrift)
	}
	return nil
}

func makeReadOnly(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.Chmod(path, info.Mode().Perm()&^0o222)
	})
	if err != nil {
		return fmt.Errorf("walk release: %w", err)
	}
	sort.Slice(directories, func(first, second int) bool { return len(directories[first]) > len(directories[second]) })
	for _, directory := range directories {
		if err := os.Chmod(directory, 0o555); err != nil {
			return fmt.Errorf("make release directory immutable: %w", err)
		}
	}
	return nil
}

func readReleaseLink(root, name string) (string, string, error) {
	target, err := os.Readlink(filepath.Join(root, name))
	if err != nil {
		return "", "", err
	}
	if !fullSHA.MatchString(target) || filepath.Base(target) != target {
		return "", "", fmt.Errorf("invalid %s release link: %w", name, ErrUnsafePath)
	}
	release := filepath.Join(root, target)
	info, err := os.Lstat(release)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("release target is not an immutable directory: %w", ErrUnsafePath)
	}
	if err := verifyImmutableDirectory(release); err != nil {
		return "", "", err
	}
	return target, release, nil
}

func verifyImmutableDirectory(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("release contains a link or special file at %s: %w", path, ErrUnsafePath)
		}
		if info.Mode().Perm()&0o222 != 0 {
			return fmt.Errorf("release path is writable at %s: %w", path, ErrDrift)
		}
		return nil
	})
}

func atomicSymlink(root, name, revision string) error {
	if !fullSHA.MatchString(revision) {
		return fmt.Errorf("invalid release revision: %w", ErrDrift)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create release root: %w", err)
	}
	if info, err := os.Lstat(filepath.Join(root, revision)); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("release revision is not materialized: %w", ErrDrift)
	}
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	temporary := filepath.Join(root, "."+name+"-"+suffix)
	if err := os.Symlink(revision, temporary); err != nil {
		return fmt.Errorf("create release link: %w", err)
	}
	if err := os.Rename(temporary, filepath.Join(root, name)); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("switch release link: %w", err)
	}
	return nil
}
