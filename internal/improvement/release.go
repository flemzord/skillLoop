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
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flemzord/skillloop/internal/domain"
)

var releaseIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var releaseLockStripes [64]sync.Mutex
var skillFenceLockStripes [64]sync.Mutex

const (
	releaseFileLockName          = ".release.lock"
	skillFenceLockName           = ".pipeline.lock"
	releaseTransitionJournalName = ".transition.json"
	maxTransitionJournalBytes    = 4 * 1024
)

// Promote materializes immutable snapshots for the exact approved revisions,
// then atomically switches the skill's current symlink to the candidate.
func (service Service) Promote(ctx context.Context, skill domain.Skill, candidate Candidate, evaluation Evaluation, approval Approval) (Promotion, error) {
	if !skill.Enabled || skill.ID == "" || skill.ID != candidate.SkillID {
		return Promotion{}, errors.New("candidate does not belong to an enabled owned skill")
	}
	rootDirectory, err := service.openSkillReleaseRoot(skill.ID)
	if err != nil {
		return Promotion{}, err
	}
	guard, err := acquireReleaseGuardAt(ctx, rootDirectory)
	if err != nil {
		_ = rootDirectory.Close()
		return Promotion{}, err
	}
	defer guard.release()
	root := rootDirectory
	publicRoot := rootDirectory.path
	if err := recoverReleaseTransitionAt(root); err != nil {
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
	diff, err := git(ctx, repository, "diff", "--no-ext-diff", "--no-textconv", candidate.BaseCommit, candidate.CandidateCommit, "--", relative)
	if err != nil || diff != candidate.Diff {
		return Promotion{}, fmt.Errorf("candidate diff moved: %w", ErrDrift)
	}

	current, currentErr := currentReleaseAt(root, skill.ID)
	if currentErr != nil && !errors.Is(currentErr, ErrNoRelease) {
		return Promotion{}, currentErr
	}
	if currentErr == nil {
		if err := verifyReleaseTargetAt(ctx, repository, root, current.Commit, relative); err != nil {
			return Promotion{}, fmt.Errorf("authenticate current release: %w", err)
		}
	}
	previousCommit := candidate.BaseCommit
	if currentErr == nil && current.Commit == candidate.CandidateCommit {
		previous, _, previousErr := readReleaseLinkAt(root, "previous")
		if previousErr != nil || previous != candidate.BaseCommit {
			return Promotion{}, fmt.Errorf("already promoted release has mismatched previous revision: %w", ErrDrift)
		}
		if err := verifyReleaseTargetAt(ctx, repository, root, previous, relative); err != nil {
			return Promotion{}, fmt.Errorf("authenticate previous release: %w", err)
		}
		if err := root.verifyIdentity(); err != nil {
			return Promotion{}, err
		}
		return Promotion{
			SkillID: skill.ID, CurrentCommit: candidate.CandidateCommit,
			PreviousCommit: candidate.BaseCommit, ReleasePath: current.Path,
			CurrentLink: filepath.Join(publicRoot, "current"), PromotedAt: time.Now().UTC(),
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
	if _, err := materializeReleaseAt(ctx, repository, root, candidate.BaseCommit, relative); err != nil {
		return Promotion{}, err
	}
	if _, err := materializeReleaseAt(ctx, repository, root, candidate.CandidateCommit, relative); err != nil {
		return Promotion{}, err
	}
	candidateRelease := filepath.Join(publicRoot, candidate.CandidateCommit)
	if err := pinRelease(ctx, repository, skill.ID, candidate.BaseCommit); err != nil {
		return Promotion{}, err
	}
	if err := pinRelease(ctx, repository, skill.ID, candidate.CandidateCommit); err != nil {
		return Promotion{}, err
	}
	if err := switchReleasePairAt(root, previousCommit, candidate.CandidateCommit); err != nil {
		return Promotion{}, err
	}
	if err := root.verifyIdentity(); err != nil {
		return Promotion{}, err
	}
	return Promotion{
		SkillID:        skill.ID,
		CurrentCommit:  candidate.CandidateCommit,
		PreviousCommit: previousCommit,
		ReleasePath:    candidateRelease,
		CurrentLink:    filepath.Join(publicRoot, "current"),
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
	rootDirectory, err := service.openSkillReleaseRoot(skill.ID)
	if err != nil {
		return Release{}, err
	}
	guard, err := acquireReleaseGuardAt(context.Background(), rootDirectory)
	if err != nil {
		_ = rootDirectory.Close()
		return Release{}, err
	}
	defer guard.release()
	root := rootDirectory
	if err := recoverReleaseTransitionAt(root); err != nil {
		return Release{}, err
	}
	release, err := currentReleaseAt(root, skill.ID)
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
	if err := verifyReleaseTargetAt(context.Background(), repository, root, release.Commit, relative); err != nil {
		return Release{}, fmt.Errorf("authenticate current release: %w", err)
	}
	if err := root.verifyIdentity(); err != nil {
		return Release{}, err
	}
	release.Path = filepath.Join(rootDirectory.path, release.Commit)
	return release, nil
}

func currentReleaseAt(root *stateDirectory, skillID string) (Release, error) {
	commit, path, err := readReleaseLinkAt(root, "current")
	if err != nil {
		if os.IsNotExist(err) {
			return Release{}, ErrNoRelease
		}
		return Release{}, err
	}
	return Release{SkillID: skillID, Commit: commit, Path: path}, nil
}

func verifyReleaseTargetAt(ctx context.Context, repository string, root *stateDirectory, revision, instruction string) error {
	directory, err := openStateChild(root, revision)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return verifyReleaseAt(ctx, repository, directory, revision, instruction)
}

// Rollback atomically switches current to the previously promoted immutable
// snapshot. The old current becomes previous, allowing an immediate undo.
func (service Service) Rollback(ctx context.Context, skill domain.Skill) (Promotion, error) {
	return service.rollback(ctx, skill, "", "")
}

// RollbackExpected switches only the exact release pair authorized by the
// caller. It closes the gap between a durable promotion decision and the
// filesystem transition even if another release operation bypasses the
// pipeline-level fence.
func (service Service) RollbackExpected(
	ctx context.Context,
	skill domain.Skill,
	expectedCurrent,
	expectedPrevious string,
) (Promotion, error) {
	if expectedCurrent == "" || expectedPrevious == "" {
		return Promotion{}, errors.New("expected current and previous releases are required")
	}
	return service.rollback(ctx, skill, expectedCurrent, expectedPrevious)
}

func (service Service) rollback(
	ctx context.Context,
	skill domain.Skill,
	expectedCurrent,
	expectedPrevious string,
) (Promotion, error) {
	rootDirectory, err := service.openSkillReleaseRoot(skill.ID)
	if err != nil {
		return Promotion{}, err
	}
	guard, err := acquireReleaseGuardAt(ctx, rootDirectory)
	if err != nil {
		_ = rootDirectory.Close()
		return Promotion{}, err
	}
	defer guard.release()
	root := rootDirectory
	if err := recoverReleaseTransitionAt(root); err != nil {
		return Promotion{}, err
	}
	currentCommit, _, err := readReleaseLinkAt(root, "current")
	if err != nil {
		if os.IsNotExist(err) {
			return Promotion{}, ErrNoRelease
		}
		return Promotion{}, err
	}
	previousCommit, _, err := readReleaseLinkAt(root, "previous")
	if err != nil {
		if os.IsNotExist(err) {
			return Promotion{}, ErrNoRelease
		}
		return Promotion{}, err
	}
	if previousCommit == currentCommit {
		return Promotion{}, errors.New("current and previous releases are identical")
	}
	if expectedCurrent != "" && (currentCommit != expectedCurrent || previousCommit != expectedPrevious) {
		return Promotion{}, fmt.Errorf("release pair changed before rollback: %w", ErrDrift)
	}
	repository, err := resolveRepository(ctx, skill.RepositoryPath)
	if err != nil {
		return Promotion{}, err
	}
	_, relative, err := resolveInstruction(repository, skill.InstructionPath)
	if err != nil {
		return Promotion{}, err
	}
	if err := verifyReleaseTargetAt(ctx, repository, root, currentCommit, relative); err != nil {
		return Promotion{}, fmt.Errorf("authenticate current release: %w", err)
	}
	if err := verifyReleaseTargetAt(ctx, repository, root, previousCommit, relative); err != nil {
		return Promotion{}, fmt.Errorf("authenticate previous release: %w", err)
	}
	if err := switchReleasePairAt(root, currentCommit, previousCommit); err != nil {
		return Promotion{}, err
	}
	if err := root.verifyIdentity(); err != nil {
		return Promotion{}, err
	}
	return Promotion{
		SkillID:        skill.ID,
		CurrentCommit:  previousCommit,
		PreviousCommit: currentCommit,
		ReleasePath:    filepath.Join(rootDirectory.path, previousCommit),
		CurrentLink:    filepath.Join(rootDirectory.path, "current"),
		RolledBack:     true,
		PromotedAt:     time.Now().UTC(),
	}, nil
}

// AcquireSkillFence serializes the complete pipeline promotion/rollback
// transaction for one skill across goroutines and processes. Release methods
// retain their narrower lock so direct callers still receive atomic symlink
// transitions.
func (service Service) AcquireSkillFence(ctx context.Context, skillID string) (func() error, error) {
	root, err := service.openSkillReleaseRoot(skillID)
	if err != nil {
		return nil, err
	}
	guard, err := acquireNamedReleaseGuardAt(ctx, root, skillFenceLockName, stripedLock(root.path, &skillFenceLockStripes))
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	var once sync.Once
	return func() error {
		once.Do(guard.release)
		return nil
	}, nil
}

func releaseLock(root string) *sync.Mutex {
	return stripedLock(root, &releaseLockStripes)
}

func stripedLock(root string, stripes *[64]sync.Mutex) *sync.Mutex {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(root))
	return &stripes[hash.Sum32()%uint32(len(stripes))]
}

type releaseGuard struct {
	file *os.File
	lock *sync.Mutex
	root *stateDirectory
}

func acquireReleaseGuardAt(ctx context.Context, root *stateDirectory) (*releaseGuard, error) {
	if root == nil {
		return nil, ErrUnsafePath
	}
	return acquireNamedReleaseGuardAt(ctx, root, releaseFileLockName, releaseLock(root.path))
}

func acquireNamedReleaseGuardAt(ctx context.Context, root *stateDirectory, lockName string, lock *sync.Mutex) (*releaseGuard, error) {
	if root == nil || root.file == nil || !safeStateComponent(lockName) {
		return nil, ErrUnsafePath
	}
	if err := root.verifyIdentity(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	lockPath := filepath.Join(root.path, lockName)
	fd, err := unix.Openat(int(root.file.Fd()), lockName, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
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
			if identityErr := root.verifyIdentity(); identityErr != nil {
				_ = unix.Flock(fd, unix.LOCK_UN)
				_ = file.Close()
				return nil, identityErr
			}
			unlocked = false
			return &releaseGuard{file: file, lock: lock, root: root}, nil
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
	if guard.root != nil {
		_ = guard.root.Close()
	}
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

func switchReleasePairAt(root *stateDirectory, newPrevious, newCurrent string) error {
	return switchReleasePairAtWithHooks(root, newPrevious, newCurrent, atomicSymlinkAt, removeReleaseTransitionJournalAt)
}

type releaseLinkSwitcherAt func(root *stateDirectory, name, revision string) error

func switchReleasePairAtWithHooks(
	root *stateDirectory,
	newPrevious,
	newCurrent string,
	switchLink releaseLinkSwitcherAt,
	removeJournal func(*stateDirectory) error,
) error {
	if err := root.verifyIdentity(); err != nil {
		return err
	}
	transition, err := snapshotReleaseTransitionAt(root)
	if err != nil {
		return err
	}
	if err := writeReleaseTransitionJournalAt(root, transition); err != nil {
		return err
	}
	recoverTransition := func(operationErr error) error {
		if recoveryErr := recoverReleaseTransitionAtWithSwitcher(root, switchLink); recoveryErr != nil {
			return errors.Join(operationErr, recoveryErr)
		}
		return operationErr
	}
	if err := root.verifyIdentity(); err != nil {
		return recoverTransition(err)
	}
	if err := switchLink(root, "previous", newPrevious); err != nil {
		return recoverTransition(err)
	}
	if err := root.verifyIdentity(); err != nil {
		return recoverTransition(err)
	}
	if err := switchLink(root, "current", newCurrent); err != nil {
		return recoverTransition(fmt.Errorf("switch current release: %w", err))
	}
	if err := root.verifyIdentity(); err != nil {
		return recoverTransition(err)
	}
	if err := removeJournal(root); err != nil {
		restoreErr := restoreReleaseTransitionAtWithSwitcher(root, transition, switchLink)
		if restoreErr != nil {
			return errors.Join(fmt.Errorf("commit release transition: %w", err), fmt.Errorf("restore stable release pair: %w", restoreErr))
		}
		return fmt.Errorf("commit release transition: %w", err)
	}
	if err := root.verifyIdentity(); err != nil {
		restoreErr := restoreReleaseTransitionAtWithSwitcher(root, transition, switchLink)
		if restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		return err
	}
	return nil
}

func snapshotReleaseTransitionAt(root *stateDirectory) (releaseTransitionJournal, error) {
	previous, previousExists, err := snapshotReleaseLinkAt(root, "previous")
	if err != nil {
		return releaseTransitionJournal{}, err
	}
	current, currentExists, err := snapshotReleaseLinkAt(root, "current")
	if err != nil {
		return releaseTransitionJournal{}, err
	}
	return releaseTransitionJournal{
		Version:  1,
		Previous: releaseTransitionLink{Exists: previousExists, Target: previous},
		Current:  releaseTransitionLink{Exists: currentExists, Target: current},
	}, nil
}

func writeReleaseTransitionJournalAt(root *stateDirectory, transition releaseTransitionJournal) error {
	if root == nil || root.file == nil {
		return ErrUnsafePath
	}
	if err := validateReleaseTransition(transition); err != nil {
		return err
	}
	probe, err := unix.Openat(int(root.file.Fd()), releaseTransitionJournalName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == nil {
		_ = unix.Close(probe)
		return fmt.Errorf("release transition already exists: %w", ErrDrift)
	}
	if !errors.Is(err, unix.ENOENT) {
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
	temporary := ".transition-" + suffix
	fd, err := unix.Openat(int(root.file.Fd()), temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create release transition: %w", err)
	}
	file := os.NewFile(uintptr(fd), temporary)
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("create release transition: invalid file descriptor")
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = unix.Unlinkat(int(root.file.Fd()), temporary, 0)
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
	if err := unix.Renameat(int(root.file.Fd()), temporary, int(root.file.Fd()), releaseTransitionJournalName); err != nil {
		return fmt.Errorf("publish release transition: %w", err)
	}
	cleanup = false
	if err := root.file.Sync(); err != nil {
		return fmt.Errorf("sync release root: %w", err)
	}
	return nil
}

func readReleaseTransitionJournalAt(root *stateDirectory) (releaseTransitionJournal, bool, error) {
	if root == nil || root.file == nil {
		return releaseTransitionJournal{}, false, ErrUnsafePath
	}
	fd, err := unix.Openat(int(root.file.Fd()), releaseTransitionJournalName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return releaseTransitionJournal{}, false, nil
	}
	if err != nil {
		return releaseTransitionJournal{}, false, fmt.Errorf("open release transition: %w", err)
	}
	file := os.NewFile(uintptr(fd), releaseTransitionJournalName)
	if file == nil {
		_ = unix.Close(fd)
		return releaseTransitionJournal{}, false, errors.New("open release transition: invalid file descriptor")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
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

func removeReleaseTransitionJournalAt(root *stateDirectory) error {
	err := unix.Unlinkat(int(root.file.Fd()), releaseTransitionJournalName, 0)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return root.file.Sync()
}

func recoverReleaseTransitionAt(root *stateDirectory) error {
	return recoverReleaseTransitionAtWithSwitcher(root, atomicSymlinkAt)
}

func recoverReleaseTransitionAtWithSwitcher(root *stateDirectory, switchLink releaseLinkSwitcherAt) error {
	transition, exists, err := readReleaseTransitionJournalAt(root)
	if err != nil || !exists {
		return err
	}
	return restoreReleaseTransitionAtWithSwitcher(root, transition, switchLink)
}

func restoreReleaseTransitionAtWithSwitcher(root *stateDirectory, transition releaseTransitionJournal, switchLink releaseLinkSwitcherAt) error {
	if err := restoreReleaseLinkAt(root, "previous", transition.Previous.Target, transition.Previous.Exists, switchLink); err != nil {
		return fmt.Errorf("restore previous release: %w", err)
	}
	if err := restoreReleaseLinkAt(root, "current", transition.Current.Target, transition.Current.Exists, switchLink); err != nil {
		return fmt.Errorf("restore current release: %w", err)
	}
	if err := removeReleaseTransitionJournalAt(root); err != nil {
		return fmt.Errorf("finish release recovery: %w", err)
	}
	return nil
}

func snapshotReleaseLinkAt(root *stateDirectory, name string) (string, bool, error) {
	buffer := make([]byte, 256)
	length, err := unix.Readlinkat(int(root.file.Fd()), name, buffer)
	if errors.Is(err, unix.ENOENT) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspect %s release link: %w", name, err)
	}
	target := string(buffer[:length])
	if length == len(buffer) || !fullSHA.MatchString(target) || filepath.Base(target) != target {
		return "", false, fmt.Errorf("invalid %s release link: %w", name, ErrUnsafePath)
	}
	return target, true, nil
}

func restoreReleaseLinkAt(root *stateDirectory, name, target string, existed bool, switchLink releaseLinkSwitcherAt) error {
	if existed {
		return switchLink(root, name, target)
	}
	err := unix.Unlinkat(int(root.file.Fd()), name, 0)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return root.file.Sync()
}

func atomicSymlinkAt(root *stateDirectory, name, revision string) error {
	if root == nil || root.file == nil || (name != "current" && name != "previous") || !fullSHA.MatchString(revision) {
		return fmt.Errorf("invalid release revision: %w", ErrDrift)
	}
	release, err := openStateChild(root, revision)
	if err != nil {
		return fmt.Errorf("release revision is not materialized: %w", ErrDrift)
	}
	_ = release.Close()
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	temporary := "." + name + "-" + suffix
	if err := unix.Symlinkat(revision, int(root.file.Fd()), temporary); err != nil {
		return fmt.Errorf("create release link: %w", err)
	}
	if err := unix.Renameat(int(root.file.Fd()), temporary, int(root.file.Fd()), name); err != nil {
		_ = unix.Unlinkat(int(root.file.Fd()), temporary, 0)
		return fmt.Errorf("switch release link: %w", err)
	}
	if err := root.file.Sync(); err != nil {
		return fmt.Errorf("sync release link: %w", err)
	}
	return nil
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

func (service Service) openSkillReleaseRoot(skillID string) (*stateDirectory, error) {
	if !releaseIdentifier.MatchString(skillID) {
		return nil, fmt.Errorf("invalid skill ID %q: %w", skillID, ErrUnsafePath)
	}
	return openStateDirectory(service.StateDir, "releases", skillID)
}

func (service Service) materializeRelease(ctx context.Context, repository string, skill domain.Skill, revision, instruction string) (string, error) {
	root, err := service.openSkillReleaseRoot(skill.ID)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if _, err := materializeReleaseAt(ctx, repository, root, revision, instruction); err != nil {
		return "", err
	}
	if err := root.verifyIdentity(); err != nil {
		return "", err
	}
	return filepath.Join(root.path, revision), nil
}

func materializeReleaseAt(ctx context.Context, repository string, root *stateDirectory, revision, instruction string) (string, error) {
	if root == nil || root.file == nil || !fullSHA.MatchString(revision) {
		return "", ErrUnsafePath
	}
	if err := root.verifyIdentity(); err != nil {
		return "", err
	}
	if err := commitExists(ctx, repository, revision); err != nil {
		return "", err
	}
	target := filepath.Join(root.path, revision)
	targetDirectory, targetErr := openStateChild(root, revision)
	if targetErr == nil {
		defer func() { _ = targetDirectory.Close() }()
		if err := verifyImmutableDirectoryAt(targetDirectory); err != nil {
			return "", err
		}
		if err := verifyReleaseAt(ctx, repository, targetDirectory, revision, instruction); err != nil {
			return "", err
		}
		if err := root.verifyIdentity(); err != nil {
			return "", err
		}
		return target, nil
	} else if !os.IsNotExist(targetErr) {
		return "", fmt.Errorf("inspect release: %w", targetErr)
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
	stagingName, staging, err := createStateChild(root, ".release-")
	if err != nil {
		return "", fmt.Errorf("create release staging directory: %w", err)
	}
	stagingFile, err := openStateChild(root, stagingName)
	if err != nil {
		_ = removeStateChildTree(root.file, stagingName)
		return "", fmt.Errorf("open release staging directory: %w", err)
	}
	stagingDirectory := &stateDirectory{path: staging, file: stagingFile}
	defer func() { _ = stagingDirectory.Close() }()
	cleanup := true
	defer func() {
		if cleanup {
			_ = removeStateChildTree(root.file, stagingName)
		}
	}()
	if err := extractArchiveAt(archive, stagingDirectory.file, stripPrefix); err != nil {
		return "", err
	}
	if err := verifyReleaseAt(ctx, repository, stagingDirectory.file, revision, instruction); err != nil {
		return "", err
	}
	if err := makeReadOnlyAt(stagingDirectory.file); err != nil {
		return "", err
	}
	if err := unix.Renameat(int(root.file.Fd()), stagingName, int(root.file.Fd()), revision); err != nil {
		if existing, openErr := openStateChild(root, revision); openErr == nil {
			defer func() { _ = existing.Close() }()
			if verifyErr := verifyReleaseAt(ctx, repository, existing, revision, instruction); verifyErr == nil {
				if immutableErr := verifyImmutableDirectoryAt(existing); immutableErr == nil {
					if identityErr := root.verifyIdentity(); identityErr == nil {
						return target, nil
					}
				}
			}
		}
		return "", fmt.Errorf("publish immutable release: %w", err)
	}
	cleanup = false
	if err := root.verifyIdentity(); err != nil {
		return "", err
	}
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

func extractArchiveAt(archive []byte, destination *os.File, stripPrefix string) error {
	return extractArchiveAtWithLimits(archive, destination, stripPrefix, releaseExtractionLimits{
		members:     maxReleaseMembers,
		memberBytes: maxReleaseMemberBytes,
		totalBytes:  maxReleaseTotalBytes,
	})
}

type releaseExtractionLimits struct {
	members     int
	memberBytes int64
	totalBytes  int64
}

func extractArchiveAtWithLimits(archive []byte, destination *os.File, stripPrefix string, limits releaseExtractionLimits) error {
	if destination == nil || limits.members <= 0 || limits.memberBytes <= 0 || limits.totalBytes <= 0 {
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
		archiveName := header.Name
		if header.Typeflag == tar.TypeDir {
			archiveName = strings.TrimSuffix(archiveName, "/")
		}
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(archiveName)))
		if clean == "." || clean != archiveName || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
			return fmt.Errorf("archive path %q escapes release: %w", header.Name, ErrUnsafePath)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			directory, err := openOrCreateStateDirectoryPath(destination, clean, true)
			if err != nil {
				return fmt.Errorf("create release directory: %w", err)
			}
			_ = directory.Close()
		case tar.TypeReg:
			file, err := createStateRegularFile(destination, clean, 0o600)
			if err != nil {
				return fmt.Errorf("create release file: %w", err)
			}
			written, copyErr := io.CopyN(file, reader, header.Size)
			if copyErr == nil && written == header.Size {
				mode := os.FileMode(header.Mode).Perm() &^ 0o222
				copyErr = file.Chmod(mode)
			}
			closeErr := file.Close()
			if copyErr != nil || written != header.Size {
				return fmt.Errorf("extract release file: %w", copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close release file: %w", closeErr)
			}
		default:
			return fmt.Errorf("archive contains link or special file %q: %w", header.Name, ErrUnsafePath)
		}
	}
}

func verifyReleaseAt(ctx context.Context, repository string, release *os.File, revision, instruction string) error {
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
	err = walkStateTree(release, func(relative string, file *os.File, info os.FileInfo) error {
		members++
		if members > maxReleaseMembers {
			return fmt.Errorf("release contains more than %d members: %w", maxReleaseMembers, ErrResourceLimit)
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
		actual, err := io.ReadAll(io.LimitReader(file, maxReleaseMemberBytes+1))
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

func makeReadOnlyAt(root *os.File) error {
	var directories []*os.File
	err := walkStateTree(root, func(_ string, file *os.File, info os.FileInfo) error {
		if info.IsDir() {
			duplicate, err := unix.Dup(int(file.Fd()))
			if err != nil {
				return err
			}
			directories = append(directories, os.NewFile(uintptr(duplicate), info.Name()))
			return nil
		}
		return file.Chmod(info.Mode().Perm() &^ 0o222)
	})
	if err != nil {
		for _, directory := range directories {
			_ = directory.Close()
		}
		return fmt.Errorf("walk release: %w", err)
	}
	for _, directory := range slices.Backward(directories) {
		if err := directory.Chmod(0o555); err != nil {
			for _, directory := range directories {
				_ = directory.Close()
			}
			return fmt.Errorf("make release directory immutable: %w", err)
		}
	}
	for _, directory := range directories {
		_ = directory.Close()
	}
	if err := root.Chmod(0o555); err != nil {
		return fmt.Errorf("make release root immutable: %w", err)
	}
	return nil
}

func readReleaseLinkAt(root *stateDirectory, name string) (string, string, error) {
	if root == nil || root.file == nil || (name != "current" && name != "previous") {
		return "", "", ErrUnsafePath
	}
	buffer := make([]byte, 256)
	length, err := unix.Readlinkat(int(root.file.Fd()), name, buffer)
	if err != nil {
		return "", "", err
	}
	target := string(buffer[:length])
	if length == len(buffer) || !fullSHA.MatchString(target) || filepath.Base(target) != target {
		return "", "", fmt.Errorf("invalid %s release link: %w", name, ErrUnsafePath)
	}
	release, err := openStateChild(root, target)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = release.Close() }()
	if err := verifyImmutableDirectoryAt(release); err != nil {
		return "", "", err
	}
	return target, filepath.Join(root.path, target), nil
}

func verifyImmutableDirectoryAt(root *os.File) error {
	members := 0
	var totalBytes int64
	foundInstruction := false
	err := walkStateTree(root, func(relative string, _ *os.File, info os.FileInfo) error {
		members++
		if members > maxReleaseMembers {
			return fmt.Errorf("release contains more than %d members: %w", maxReleaseMembers, ErrResourceLimit)
		}
		parts := strings.Split(relative, "/")
		if parts[0] != "SKILL.md" && parts[0] != "scripts" && parts[0] != "assets" {
			return fmt.Errorf("release contains unowned path %q: %w", relative, ErrUnsafePath)
		}
		if parts[0] == "SKILL.md" {
			if len(parts) != 1 || info.IsDir() {
				return fmt.Errorf("release has invalid SKILL.md path: %w", ErrUnsafePath)
			}
			foundInstruction = true
		} else if len(parts) == 1 && !info.IsDir() {
			return fmt.Errorf("release path %q must be a directory: %w", relative, ErrUnsafePath)
		}
		if info.Mode().Perm()&0o222 != 0 {
			return fmt.Errorf("release path is writable at %s: %w", relative, ErrDrift)
		}
		if info.Mode().IsRegular() {
			if info.Size() < 0 || info.Size() > maxReleaseMemberBytes || info.Size() > maxReleaseTotalBytes-totalBytes {
				return fmt.Errorf("release path exceeds configured size limits at %s: %w", relative, ErrResourceLimit)
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
