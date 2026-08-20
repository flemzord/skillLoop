package improvement

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flemzord/skillloop/internal/domain"
)

var releaseIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var releaseLockStripes [64]sync.Mutex

const (
	releaseFileLockName          = ".release.lock"
	releaseTransitionJournalName = ".transition.json"
	maxTransitionJournalBytes    = 4 * 1024
)

// Promote materializes immutable snapshots for the exact approved revisions,
// then atomically switches the skill's current symlink to the candidate.
func (service Service) Promote(ctx context.Context, skill domain.Skill, candidate Candidate, evaluation Evaluation, approval Approval) (Promotion, error) {
	if !skill.Enabled || skill.ID == "" || skill.ID != candidate.SkillID {
		return Promotion{}, errors.New("candidate does not belong to an enabled owned skill")
	}
	root, err := service.skillReleaseRoot(skill.ID)
	if err != nil {
		return Promotion{}, err
	}
	guard, err := acquireReleaseGuard(ctx, root)
	if err != nil {
		return Promotion{}, err
	}
	defer guard.release()
	if err := recoverReleaseTransition(root, atomicSymlink); err != nil {
		return Promotion{}, err
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

	current, currentErr := currentRelease(root, skill.ID)
	if currentErr != nil && !errors.Is(currentErr, ErrNoRelease) {
		return Promotion{}, currentErr
	}
	if currentErr == nil {
		if err := verifyRelease(ctx, repository, current.Path, current.Commit, relative); err != nil {
			return Promotion{}, fmt.Errorf("authenticate current release: %w", err)
		}
	}
	previousCommit := candidate.BaseCommit
	if currentErr == nil && current.Commit == candidate.CandidateCommit {
		root, rootErr := service.skillReleaseRoot(skill.ID)
		if rootErr != nil {
			return Promotion{}, rootErr
		}
		previous, previousPath, previousErr := readReleaseLink(root, "previous")
		if previousErr != nil || previous != candidate.BaseCommit {
			return Promotion{}, fmt.Errorf("already promoted release has mismatched previous revision: %w", ErrDrift)
		}
		if err := verifyRelease(ctx, repository, previousPath, previous, relative); err != nil {
			return Promotion{}, fmt.Errorf("authenticate previous release: %w", err)
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
	if err := pinRelease(ctx, repository, skill.ID, candidate.BaseCommit); err != nil {
		return Promotion{}, err
	}
	if err := pinRelease(ctx, repository, skill.ID, candidate.CandidateCommit); err != nil {
		return Promotion{}, err
	}
	if err := switchReleasePair(root, previousCommit, candidate.CandidateCommit, atomicSymlink); err != nil {
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
	guard, err := acquireReleaseGuard(context.Background(), root)
	if err != nil {
		return Release{}, err
	}
	defer guard.release()
	if err := recoverReleaseTransition(root, atomicSymlink); err != nil {
		return Release{}, err
	}
	release, err := currentRelease(root, skill.ID)
	if err != nil {
		return Release{}, err
	}
	repository, err := resolveRepository(context.Background(), skill.RepositoryPath)
	if err != nil {
		return Release{}, err
	}
	_, relative, err := resolveInstruction(repository, skill.InstructionPath)
	if err != nil {
		return Release{}, err
	}
	if err := verifyRelease(context.Background(), repository, release.Path, release.Commit, relative); err != nil {
		return Release{}, fmt.Errorf("authenticate current release: %w", err)
	}
	return release, nil
}

func currentRelease(root, skillID string) (Release, error) {
	commit, path, err := readReleaseLink(root, "current")
	if err != nil {
		if os.IsNotExist(err) {
			return Release{}, ErrNoRelease
		}
		return Release{}, err
	}
	return Release{SkillID: skillID, Commit: commit, Path: path}, nil
}

// Rollback atomically switches current to the previously promoted immutable
// snapshot. The old current becomes previous, allowing an immediate undo.
func (service Service) Rollback(ctx context.Context, skill domain.Skill) (Promotion, error) {
	root, err := service.skillReleaseRoot(skill.ID)
	if err != nil {
		return Promotion{}, err
	}
	guard, err := acquireReleaseGuard(ctx, root)
	if err != nil {
		return Promotion{}, err
	}
	defer guard.release()
	if err := recoverReleaseTransition(root, atomicSymlink); err != nil {
		return Promotion{}, err
	}
	currentCommit, currentPath, err := readReleaseLink(root, "current")
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
	repository, err := resolveRepository(ctx, skill.RepositoryPath)
	if err != nil {
		return Promotion{}, err
	}
	_, relative, err := resolveInstruction(repository, skill.InstructionPath)
	if err != nil {
		return Promotion{}, err
	}
	if err := verifyRelease(ctx, repository, currentPath, currentCommit, relative); err != nil {
		return Promotion{}, fmt.Errorf("authenticate current release: %w", err)
	}
	if err := verifyRelease(ctx, repository, previousPath, previousCommit, relative); err != nil {
		return Promotion{}, fmt.Errorf("authenticate previous release: %w", err)
	}
	if err := switchReleasePair(root, currentCommit, previousCommit, atomicSymlink); err != nil {
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

func releaseLock(root string) *sync.Mutex {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(root))
	return &releaseLockStripes[hash.Sum32()%uint32(len(releaseLockStripes))]
}

type releaseGuard struct {
	file *os.File
	lock *sync.Mutex
}

func acquireReleaseGuard(ctx context.Context, root string) (*releaseGuard, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock := releaseLock(root)
	for !lock.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		lock.Unlock()
		return nil, err
	}
	unlocked := true
	defer func() {
		if unlocked {
			lock.Unlock()
		}
	}()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create release root: %w", err)
	}
	lockPath := filepath.Join(root, releaseFileLockName)
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open release lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), lockPath)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open release lock: invalid file descriptor")
	}
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			if contextErr := ctx.Err(); contextErr != nil {
				_ = unix.Flock(fd, unix.LOCK_UN)
				_ = file.Close()
				return nil, contextErr
			}
			unlocked = false
			return &releaseGuard{file: file, lock: lock}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock release state: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (guard *releaseGuard) release() {
	if guard == nil {
		return
	}
	if guard.file != nil {
		_ = unix.Flock(int(guard.file.Fd()), unix.LOCK_UN)
		_ = guard.file.Close()
	}
	if guard.lock != nil {
		guard.lock.Unlock()
	}
}

type releaseLinkSwitcher func(root, name, revision string) error

func switchReleasePair(root, newPrevious, newCurrent string, switchLink releaseLinkSwitcher) error {
	return switchReleasePairWithJournal(root, newPrevious, newCurrent, switchLink, removeReleaseTransitionJournal)
}

func switchReleasePairWithJournal(
	root, newPrevious, newCurrent string,
	switchLink releaseLinkSwitcher,
	removeJournal func(string) error,
) error {
	transition, err := snapshotReleaseTransition(root)
	if err != nil {
		return err
	}
	if err := writeReleaseTransitionJournal(root, transition); err != nil {
		return err
	}
	if err := switchLink(root, "previous", newPrevious); err != nil {
		if recoveryErr := recoverReleaseTransition(root, switchLink); recoveryErr != nil {
			return errors.Join(err, recoveryErr)
		}
		return err
	}
	if err := switchLink(root, "current", newCurrent); err != nil {
		restoreErr := recoverReleaseTransition(root, switchLink)
		if restoreErr != nil {
			return errors.Join(fmt.Errorf("switch current release: %w", err), fmt.Errorf("restore stable release pair: %w", restoreErr))
		}
		return fmt.Errorf("switch current release: %w", err)
	}
	if err := removeJournal(root); err != nil {
		restoreErr := restoreReleaseTransition(root, transition, switchLink)
		if restoreErr != nil {
			return errors.Join(fmt.Errorf("commit release transition: %w", err), fmt.Errorf("restore stable release pair: %w", restoreErr))
		}
		return fmt.Errorf("commit release transition: %w", err)
	}
	return nil
}

type releaseTransitionLink struct {
	Exists bool   `json:"exists"`
	Target string `json:"target,omitempty"`
}

type releaseTransitionJournal struct {
	Version  int                   `json:"version"`
	Previous releaseTransitionLink `json:"previous"`
	Current  releaseTransitionLink `json:"current"`
}

func snapshotReleaseTransition(root string) (releaseTransitionJournal, error) {
	previous, previousExists, err := snapshotReleaseLink(root, "previous")
	if err != nil {
		return releaseTransitionJournal{}, err
	}
	current, currentExists, err := snapshotReleaseLink(root, "current")
	if err != nil {
		return releaseTransitionJournal{}, err
	}
	return releaseTransitionJournal{
		Version:  1,
		Previous: releaseTransitionLink{Exists: previousExists, Target: previous},
		Current:  releaseTransitionLink{Exists: currentExists, Target: current},
	}, nil
}

func writeReleaseTransitionJournal(root string, transition releaseTransitionJournal) error {
	if err := validateReleaseTransition(transition); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(root, releaseTransitionJournalName)); err == nil {
		return fmt.Errorf("release transition already exists: %w", ErrDrift)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect release transition: %w", err)
	}
	payload, err := json.Marshal(transition)
	if err != nil {
		return fmt.Errorf("encode release transition: %w", err)
	}
	if len(payload) > maxTransitionJournalBytes {
		return fmt.Errorf("release transition journal is too large: %w", ErrResourceLimit)
	}
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	temporary := filepath.Join(root, ".transition-"+suffix)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create release transition: %w", err)
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return fmt.Errorf("write release transition: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync release transition: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close release transition: %w", err)
	}
	if err := os.Rename(temporary, filepath.Join(root, releaseTransitionJournalName)); err != nil {
		return fmt.Errorf("publish release transition: %w", err)
	}
	cleanup = false
	if err := syncDirectory(root); err != nil {
		return fmt.Errorf("sync release root: %w", err)
	}
	return nil
}

func recoverReleaseTransition(root string, switchLink releaseLinkSwitcher) error {
	transition, exists, err := readReleaseTransitionJournal(root)
	if err != nil || !exists {
		return err
	}
	return restoreReleaseTransition(root, transition, switchLink)
}

func restoreReleaseTransition(root string, transition releaseTransitionJournal, switchLink releaseLinkSwitcher) error {
	if err := restoreReleaseLink(root, "previous", transition.Previous.Target, transition.Previous.Exists, switchLink); err != nil {
		return fmt.Errorf("restore previous release: %w", err)
	}
	if err := restoreReleaseLink(root, "current", transition.Current.Target, transition.Current.Exists, switchLink); err != nil {
		return fmt.Errorf("restore current release: %w", err)
	}
	if err := removeReleaseTransitionJournal(root); err != nil {
		return fmt.Errorf("finish release recovery: %w", err)
	}
	return nil
}

func readReleaseTransitionJournal(root string) (releaseTransitionJournal, bool, error) {
	path := filepath.Join(root, releaseTransitionJournalName)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return releaseTransitionJournal{}, false, nil
	}
	if err != nil {
		return releaseTransitionJournal{}, false, fmt.Errorf("open release transition: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return releaseTransitionJournal{}, false, errors.New("open release transition: invalid file descriptor")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return releaseTransitionJournal{}, false, fmt.Errorf("inspect release transition: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return releaseTransitionJournal{}, false, fmt.Errorf("release transition is not a regular file: %w", ErrUnsafePath)
	}
	if info.Size() < 0 || info.Size() > maxTransitionJournalBytes {
		return releaseTransitionJournal{}, false, fmt.Errorf("release transition journal is too large: %w", ErrResourceLimit)
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxTransitionJournalBytes+1))
	decoder.DisallowUnknownFields()
	var transition releaseTransitionJournal
	if err := decoder.Decode(&transition); err != nil {
		return releaseTransitionJournal{}, false, fmt.Errorf("decode release transition: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return releaseTransitionJournal{}, false, fmt.Errorf("decode release transition trailing data: %w", ErrUnsafePath)
	}
	if err := validateReleaseTransition(transition); err != nil {
		return releaseTransitionJournal{}, false, err
	}
	return transition, true, nil
}

func validateReleaseTransition(transition releaseTransitionJournal) error {
	if transition.Version != 1 {
		return fmt.Errorf("unsupported release transition version: %w", ErrUnsafePath)
	}
	for name, link := range map[string]releaseTransitionLink{"previous": transition.Previous, "current": transition.Current} {
		if link.Exists {
			if !fullSHA.MatchString(link.Target) || filepath.Base(link.Target) != link.Target {
				return fmt.Errorf("invalid %s transition target: %w", name, ErrUnsafePath)
			}
		} else if link.Target != "" {
			return fmt.Errorf("unexpected absent %s transition target: %w", name, ErrUnsafePath)
		}
	}
	return nil
}

func removeReleaseTransitionJournal(root string) error {
	if err := os.Remove(filepath.Join(root, releaseTransitionJournalName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDirectory(root)
}

func snapshotReleaseLink(root, name string) (string, bool, error) {
	target, err := os.Readlink(filepath.Join(root, name))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspect %s release link: %w", name, err)
	}
	if !fullSHA.MatchString(target) || filepath.Base(target) != target {
		return "", false, fmt.Errorf("invalid %s release link: %w", name, ErrUnsafePath)
	}
	return target, true, nil
}

func restoreReleaseLink(root, name, target string, existed bool, switchLink releaseLinkSwitcher) error {
	if existed {
		return switchLink(root, name, target)
	}
	if err := os.Remove(filepath.Join(root, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDirectory(root)
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
		if err := verifyImmutableDirectory(target); err != nil {
			return "", err
		}
		if err := verifyRelease(ctx, repository, target, revision, instruction); err != nil {
			return "", err
		}
		return target, nil
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("inspect release: %w", statErr)
	}

	ownedPaths, err := releaseArchivePaths(ctx, repository, revision, instruction)
	if err != nil {
		return "", err
	}
	skillDirectory := filepath.ToSlash(filepath.Dir(instruction))
	archiveArgs := []string{"archive", "--format=tar", revision, "--"}
	archiveArgs = append(archiveArgs, ownedPaths...)
	stripPrefix := ""
	if skillDirectory != "." {
		stripPrefix = strings.TrimSuffix(skillDirectory, "/") + "/"
	}
	archive, err := gitBytesLimit(ctx, repository, maxReleaseArchiveBytes, archiveArgs...)
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

func releaseArchivePaths(ctx context.Context, repository, revision, instruction string) ([]string, error) {
	paths := []string{instruction}
	directory := filepath.ToSlash(filepath.Dir(instruction))
	for _, ownedDirectory := range []string{"scripts", "assets"} {
		candidate := ownedDirectory
		if directory != "." {
			candidate = directory + "/" + ownedDirectory
		}
		entry, err := gitBytesLimit(ctx, repository, 64*1024, "ls-tree", "-d", "--name-only", "-z", revision, "--", candidate)
		if err != nil {
			return nil, err
		}
		if len(entry) == 0 {
			continue
		}
		if string(entry) != candidate+"\x00" {
			return nil, fmt.Errorf("unexpected owned skill tree entry %q: %w", entry, ErrUnsafePath)
		}
		paths = append(paths, candidate)
	}
	return paths, nil
}

func extractArchive(archive []byte, destination, stripPrefix string) error {
	return extractArchiveWithLimits(archive, destination, stripPrefix, archiveLimits{
		members:     maxReleaseMembers,
		memberBytes: maxReleaseMemberBytes,
		totalBytes:  maxReleaseTotalBytes,
	})
}

type archiveLimits struct {
	members     int
	memberBytes int64
	totalBytes  int64
}

func extractArchiveWithLimits(archive []byte, destination, stripPrefix string, limits archiveLimits) error {
	if limits.members <= 0 || limits.memberBytes <= 0 || limits.totalBytes <= 0 {
		return ErrResourceLimit
	}
	reader := tar.NewReader(bytes.NewReader(archive))
	members := 0
	var totalBytes int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read git archive: %w", err)
		}
		members++
		if members > limits.members {
			return fmt.Errorf("release archive contains more than %d members: %w", limits.members, ErrResourceLimit)
		}
		if header.Size < 0 || header.Size > limits.memberBytes || header.Size > limits.totalBytes-totalBytes {
			return fmt.Errorf("release member %q exceeds configured size limits: %w", header.Name, ErrResourceLimit)
		}
		totalBytes += header.Size
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
			written, copyErr := io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			if copyErr != nil || written != header.Size {
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
	expected, err := expectedReleaseManifest(ctx, repository, revision, instruction)
	if err != nil {
		return err
	}
	expectedDirectories := make(map[string]struct{})
	for path := range expected {
		for directory := filepath.ToSlash(filepath.Dir(path)); directory != "."; directory = filepath.ToSlash(filepath.Dir(directory)) {
			expectedDirectories[directory] = struct{}{}
		}
	}
	actualFiles := 0
	members := 0
	var totalBytes int64
	err = filepath.WalkDir(release, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == release {
			return nil
		}
		members++
		if members > maxReleaseMembers {
			return fmt.Errorf("release contains more than %d members: %w", maxReleaseMembers, ErrResourceLimit)
		}
		relative, err := filepath.Rel(release, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("release contains a link or special file at %s: %w", relative, ErrUnsafePath)
		}
		if info.IsDir() {
			if _, found := expectedDirectories[relative]; !found {
				return fmt.Errorf("release contains directory absent from commit at %s: %w", relative, ErrDrift)
			}
			return nil
		}
		manifestEntry, found := expected[relative]
		if !found {
			return fmt.Errorf("release contains file absent from commit at %s: %w", relative, ErrDrift)
		}
		actualFiles++
		if info.Mode().Perm() != manifestEntry.mode {
			return fmt.Errorf("released file mode differs from commit at %s: %w", relative, ErrDrift)
		}
		if info.Size() < 0 || info.Size() > maxReleaseMemberBytes || info.Size() > maxReleaseTotalBytes-totalBytes {
			return fmt.Errorf("released file %s exceeds configured size limits: %w", relative, ErrResourceLimit)
		}
		totalBytes += info.Size()
		if info.Size() != int64(len(manifestEntry.contents)) {
			return fmt.Errorf("released file size differs from commit at %s: %w", relative, ErrDrift)
		}
		actual, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read released file %s: %w", relative, err)
		}
		if !bytes.Equal(manifestEntry.contents, actual) {
			return fmt.Errorf("released file contents differ from commit at %s: %w", relative, ErrDrift)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if actualFiles != len(expected) {
		return fmt.Errorf("release is missing files from commit: %w", ErrDrift)
	}
	return nil
}

type releaseManifestEntry struct {
	contents []byte
	mode     os.FileMode
}

func expectedReleaseManifest(ctx context.Context, repository, revision, instruction string) (map[string]releaseManifestEntry, error) {
	ownedPaths, err := releaseArchivePaths(ctx, repository, revision, instruction)
	if err != nil {
		return nil, err
	}
	args := []string{"ls-tree", "-r", "-z", "--full-tree", revision, "--"}
	args = append(args, ownedPaths...)
	tree, err := gitBytesLimit(ctx, repository, maxReleaseArchiveBytes, args...)
	if err != nil {
		return nil, err
	}
	directory := filepath.ToSlash(filepath.Dir(instruction))
	prefix := ""
	if directory != "." {
		prefix = strings.TrimSuffix(directory, "/") + "/"
	}
	manifest := make(map[string]releaseManifestEntry)
	var totalBytes int64
	for record := range bytes.SplitSeq(tree, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		metadata, pathBytes, found := bytes.Cut(record, []byte{'\t'})
		fields := bytes.Fields(metadata)
		if !found || len(fields) != 3 || string(fields[1]) != "blob" {
			return nil, fmt.Errorf("unexpected owned skill tree entry %q: %w", record, ErrUnsafePath)
		}
		mode := os.FileMode(0o444)
		switch string(fields[0]) {
		case "100644":
		case "100755":
			mode = 0o555
		default:
			return nil, fmt.Errorf("owned skill path is not a regular file: %w", ErrUnsafePath)
		}
		object := string(fields[2])
		if len(object) < 40 || len(object) > 64 {
			return nil, fmt.Errorf("invalid owned skill object ID: %w", ErrUnsafePath)
		}
		for _, character := range object {
			if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
				return nil, fmt.Errorf("invalid owned skill object ID: %w", ErrUnsafePath)
			}
		}
		path := string(pathBytes)
		if prefix != "" {
			if !strings.HasPrefix(path, prefix) {
				return nil, fmt.Errorf("owned skill path %q escapes its directory: %w", path, ErrUnsafePath)
			}
			path = strings.TrimPrefix(path, prefix)
		}
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
		if clean != path || clean == "." || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
			return nil, fmt.Errorf("invalid owned skill path %q: %w", path, ErrUnsafePath)
		}
		if path != "SKILL.md" && !strings.HasPrefix(path, "scripts/") && !strings.HasPrefix(path, "assets/") {
			return nil, fmt.Errorf("unowned path %q in release manifest: %w", path, ErrUnsafePath)
		}
		if _, duplicate := manifest[path]; duplicate {
			return nil, fmt.Errorf("duplicate release path %q: %w", path, ErrUnsafePath)
		}
		if len(manifest) >= maxReleaseMembers {
			return nil, fmt.Errorf("release manifest contains more than %d files: %w", maxReleaseMembers, ErrResourceLimit)
		}
		contents, err := gitBytesLimit(ctx, repository, maxReleaseMemberBytes, "cat-file", "blob", object)
		if err != nil {
			return nil, err
		}
		if int64(len(contents)) > maxReleaseTotalBytes-totalBytes {
			return nil, fmt.Errorf("owned skill files exceed configured size limits: %w", ErrResourceLimit)
		}
		totalBytes += int64(len(contents))
		manifest[path] = releaseManifestEntry{contents: contents, mode: mode}
	}
	if _, found := manifest["SKILL.md"]; !found {
		return nil, fmt.Errorf("release manifest is missing SKILL.md: %w", ErrDrift)
	}
	return manifest, nil
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
	members := 0
	var totalBytes int64
	foundInstruction := false
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		members++
		if members > maxReleaseMembers {
			return fmt.Errorf("release contains more than %d members: %w", maxReleaseMembers, ErrResourceLimit)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if parts[0] != "SKILL.md" && parts[0] != "scripts" && parts[0] != "assets" {
			return fmt.Errorf("release contains unowned path %q: %w", relative, ErrUnsafePath)
		}
		if parts[0] == "SKILL.md" {
			if len(parts) != 1 || entry.IsDir() {
				return fmt.Errorf("release has invalid SKILL.md path: %w", ErrUnsafePath)
			}
			foundInstruction = true
		} else if len(parts) == 1 && !entry.IsDir() {
			return fmt.Errorf("release path %q must be a directory: %w", relative, ErrUnsafePath)
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
		if info.Mode().IsRegular() {
			if info.Size() < 0 || info.Size() > maxReleaseMemberBytes || info.Size() > maxReleaseTotalBytes-totalBytes {
				return fmt.Errorf("release path exceeds configured size limits at %s: %w", path, ErrResourceLimit)
			}
			totalBytes += info.Size()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !foundInstruction {
		return fmt.Errorf("release is missing SKILL.md: %w", ErrDrift)
	}
	return nil
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
	if err := syncDirectory(root); err != nil {
		return fmt.Errorf("sync release link: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}
