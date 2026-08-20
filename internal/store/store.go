package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"

	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/sanitize"
)

var ErrNotFound = errors.New("store: not found")

type Counts struct {
	Sessions         int
	Skills           int
	Cards            int
	Clusters         int
	EligibleClusters int
	ActivePromotions int
	Rollbacks        int
	Jobs             map[domain.JobStatus]int
	Proposals        map[domain.ProposalStatus]int
}

type RetentionPruneResult struct {
	TranscriptLocators int
	CompletedJobs      int
	FailedJobs         int
}

// Store persists SkillLoop's durable metadata. Transcript contents deliberately
// remain outside the database; sessions only retain their original locator.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("store: database path is required")
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve database path: %w", err)
	}
	databaseName := filepath.Base(absPath)
	if !safeStateName(databaseName) {
		return nil, errors.New("store: database filename is unsafe")
	}
	stateDir, stateDirFD, err := openPrivateStateDirectory(filepath.Dir(absPath))
	if err != nil {
		return nil, fmt.Errorf("store: secure database directory: %w", err)
	}
	defer func() { _ = unix.Close(stateDirFD) }()
	databasePath := filepath.Join(stateDir, databaseName)
	if err := secureSQLiteSidecars(stateDirFD, databaseName); err != nil {
		return nil, err
	}
	databaseIdentity, exists, err := secureStateFile(stateDirFD, databaseName, true)
	if err != nil {
		return nil, fmt.Errorf("store: secure database file: %w", err)
	}
	if !exists {
		return nil, errors.New("store: database file was not created")
	}
	if err := unix.Fsync(stateDirFD); err != nil {
		return nil, fmt.Errorf("store: sync database directory: %w", err)
	}

	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "journal_mode(WAL)")
	dsn := (&url.URL{Scheme: "file", Path: databasePath, RawQuery: query.Encode()}).String()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	// SkillLoop has one local daemon. Serializing access avoids SQLITE_BUSY while
	// WAL still provides crash-safe, restartable persistence.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db, now: time.Now}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping database: %w", err)
	}
	if err := verifySQLiteState(stateDirFD, databaseName, databaseIdentity); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := verifySQLiteState(stateDirFD, databaseName, databaseIdentity); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func openPrivateStateDirectory(path string) (string, int, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", -1, err
	}
	absolute = filepath.Clean(absolute)
	if absolute == string(filepath.Separator) {
		return "", -1, errors.New("database directory cannot be the filesystem root")
	}
	if info, statErr := os.Lstat(absolute); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", -1, errors.New("database directory cannot be a symlink")
		}
		if !info.IsDir() {
			return "", -1, errors.New("database directory is not a directory")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", -1, statErr
	}
	absolute, err = canonicalizeStateParent(absolute)
	if err != nil {
		return "", -1, err
	}

	current, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", -1, err
	}
	currentPath := string(filepath.Separator)
	if err := validateStateDirectoryAnchor(current, currentPath); err != nil {
		_ = unix.Close(current)
		return "", -1, err
	}
	components := strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator))
	for index, component := range components {
		if !safeStateName(component) {
			_ = unix.Close(current)
			return "", -1, errors.New("database directory contains an unsafe path component")
		}
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(current, component, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(current)
				return "", -1, mkdirErr
			}
			next, openErr = unix.Openat(current, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		}
		_ = unix.Close(current)
		if openErr != nil {
			return "", -1, fmt.Errorf("open database directory component %q without symlinks: %w", component, openErr)
		}
		current = next
		currentPath = filepath.Join(currentPath, component)
		if err := validateStateDirectoryAnchor(current, currentPath); err != nil {
			_ = unix.Close(current)
			return "", -1, err
		}
		if index == len(components)-1 {
			if err := unix.Fchmod(current, 0o700); err != nil {
				_ = unix.Close(current)
				return "", -1, err
			}
		}
	}
	return absolute, current, nil
}

func validateStateDirectoryAnchor(fd int, path string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect database directory ancestor %q: %w", path, err)
	}
	if stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("database directory ancestor %q is owned by another uid", path)
	}
	mode := uint32(stat.Mode)
	groupCanRename := mode&unix.S_IWGRP != 0 && mode&unix.S_IXGRP != 0
	worldCanRename := mode&unix.S_IWOTH != 0 && mode&unix.S_IXOTH != 0
	if (groupCanRename || worldCanRename) && mode&unix.S_ISVTX == 0 {
		return fmt.Errorf("database directory ancestor %q is writable by another uid without sticky protection", path)
	}
	return nil
}

func canonicalizeStateParent(path string) (string, error) {
	// Resolve only ancestors. The requested state directory itself must still be
	// opened with O_NOFOLLOW below, including if it is swapped after the Lstat.
	missing := []string{filepath.Base(path)}
	current := filepath.Dir(path)
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", resolveErr
			}
			for index := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("store: no existing database directory ancestor")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func verifySQLiteState(directoryFD int, databaseName string, expected os.FileInfo) error {
	current, exists, err := secureStateFile(directoryFD, databaseName, false)
	if err != nil {
		return fmt.Errorf("store: verify database file: %w", err)
	}
	if !exists || !os.SameFile(expected, current) {
		return errors.New("store: database file changed while open")
	}
	if err := secureSQLiteSidecars(directoryFD, databaseName); err != nil {
		return err
	}
	return nil
}

func secureSQLiteSidecars(directoryFD int, databaseName string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, _, err := secureStateFile(directoryFD, databaseName+suffix, false); err != nil {
			return fmt.Errorf("store: secure SQLite sidecar %s: %w", suffix, err)
		}
	}
	return nil
}

func secureStateFile(directoryFD int, name string, create bool) (os.FileInfo, bool, error) {
	if !safeStateName(name) {
		return nil, false, errors.New("unsafe state filename")
	}
	var before unix.Stat_t
	statErr := unix.Fstatat(directoryFD, name, &before, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(statErr, unix.ENOENT) {
		if !create {
			return nil, false, nil
		}
	} else if statErr != nil {
		return nil, false, statErr
	} else if err := validateStateStat(before); err != nil {
		return nil, false, err
	}

	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if create {
		flags |= unix.O_CREAT
	}
	fd, err := unix.Openat(directoryFD, name, flags, 0o600)
	if errors.Is(err, unix.ENOENT) && !create {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, false, errors.New("invalid state file descriptor")
	}
	defer func() { _ = file.Close() }()
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return nil, false, err
	}
	if err := validateStateStat(opened); err != nil {
		return nil, false, err
	}
	if statErr == nil && (before.Dev != opened.Dev || before.Ino != opened.Ino) {
		return nil, false, errors.New("state file changed while opening")
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, false, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	return info, true, nil
}

func validateStateStat(stat unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("state entry is not a regular file")
	}
	if stat.Nlink != 1 {
		return errors.New("state file must have exactly one link")
	}
	return nil
}

func safeStateName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 &&
		!strings.ContainsRune(name, filepath.Separator) && filepath.Base(name) == name
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) RegisterSkill(ctx context.Context, skill domain.Skill) (bool, error) {
	if skill.ID == "" || skill.Name == "" || skill.RepositoryPath == "" || skill.InstructionPath == "" {
		return false, errors.New("store: skill id, name, repository path, and instruction path are required")
	}
	createdAt := timeOrNow(skill.CreatedAt, s.now)
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO skills (id, name, repository_path, instruction_path, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		skill.ID, skill.Name, skill.RepositoryPath, skill.InstructionPath, boolInt(skill.Enabled), unixNano(createdAt),
	)
	if err != nil {
		return false, fmt.Errorf("store: register skill: %w", err)
	}
	return rowsChanged(result)
}

func (s *Store) ListSkills(ctx context.Context) ([]domain.Skill, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, repository_path, instruction_path, enabled, created_at
		FROM skills ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list skills: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var skills []domain.Skill
	for rows.Next() {
		var skill domain.Skill
		var enabled int
		var createdAt int64
		if err := rows.Scan(&skill.ID, &skill.Name, &skill.RepositoryPath, &skill.InstructionPath, &enabled, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan skill: %w", err)
		}
		skill.Enabled = enabled != 0
		skill.CreatedAt = fromUnixNano(createdAt)
		skills = append(skills, skill)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate skills: %w", err)
	}
	return skills, nil
}

func (s *Store) Skill(ctx context.Context, id string) (domain.Skill, error) {
	var skill domain.Skill
	var enabled int
	var createdAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, repository_path, instruction_path, enabled, created_at
		FROM skills WHERE id = ?`, id,
	).Scan(&skill.ID, &skill.Name, &skill.RepositoryPath, &skill.InstructionPath, &enabled, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Skill{}, ErrNotFound
	}
	if err != nil {
		return domain.Skill{}, fmt.Errorf("store: get skill: %w", err)
	}
	skill.Enabled = enabled != 0
	skill.CreatedAt = fromUnixNano(createdAt)
	return skill, nil
}

func (s *Store) RecordSession(ctx context.Context, session domain.Session) (bool, error) {
	return s.recordSession(ctx, s.db, session)
}

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) recordSession(ctx context.Context, executor sqlExecutor, session domain.Session) (bool, error) {
	if session.Reference == "" || !session.Source.Valid() || session.ExternalID == "" {
		return false, errors.New("store: session reference, valid source, and external id are required")
	}
	if session.Outcome == "" {
		session.Outcome = domain.SessionOutcomeUnknown
	}
	if !session.Outcome.Valid() {
		return false, fmt.Errorf("store: invalid session outcome %q", session.Outcome)
	}
	now := s.now()
	binding := sessionTranscriptBinding(session)
	result, err := executor.ExecContext(ctx, `
		INSERT INTO sessions (
			reference, source, external_id, turn_id, working_dir, transcript_path, transcript_binding,
			outcome, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source, external_id) DO NOTHING`,
		session.Reference, session.Source, session.ExternalID, session.TurnID, session.WorkingDir,
		session.TranscriptPath, binding, session.Outcome, unixNano(now), unixNano(now),
	)
	if err != nil {
		return false, fmt.Errorf("store: record session: %w", err)
	}
	created, err := rowsChanged(result)
	if err != nil {
		return false, err
	}
	if created {
		return true, nil
	}
	var storedReference, storedWorkingDir, storedTranscriptPath, storedBinding string
	if err := executor.QueryRowContext(ctx,
		`SELECT reference, working_dir, transcript_path, transcript_binding
		 FROM sessions WHERE source = ? AND external_id = ?`,
		session.Source, session.ExternalID,
	).Scan(&storedReference, &storedWorkingDir, &storedTranscriptPath, &storedBinding); err != nil {
		return false, fmt.Errorf("store: resolve existing session: %w", err)
	}
	if storedReference != session.Reference {
		return false, fmt.Errorf(
			"store: session %s/%s already uses reference %q",
			session.Source, session.ExternalID, storedReference,
		)
	}
	if cleanSessionPath(storedWorkingDir) != cleanSessionPath(session.WorkingDir) {
		return false, errors.New("store: existing session working directory does not match replay")
	}
	if storedBinding == "" {
		storedSession := session
		storedSession.TranscriptPath = storedTranscriptPath
		if sessionTranscriptBinding(storedSession) != binding {
			return false, errors.New("store: existing session transcript binding does not match replay")
		}
	} else if storedBinding != binding {
		return false, errors.New("store: existing session transcript binding does not match replay")
	}

	result, err = executor.ExecContext(ctx, `
		UPDATE sessions SET
			turn_id = CASE WHEN ? <> '' THEN ? ELSE turn_id END,
			transcript_binding = CASE WHEN transcript_binding = '' THEN ? ELSE transcript_binding END,
			outcome = CASE WHEN ? <> 'unknown' THEN ? ELSE outcome END,
			updated_at = ?
		WHERE source = ? AND external_id = ?`,
		session.TurnID, session.TurnID,
		binding,
		session.Outcome, session.Outcome,
		unixNano(now), session.Source, session.ExternalID,
	)
	if err != nil {
		return false, fmt.Errorf("store: refresh session: %w", err)
	}
	changed, err := rowsChanged(result)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, errors.New("store: session conflict could not be refreshed")
	}
	return false, nil
}

func sessionTranscriptBinding(session domain.Session) string {
	transcriptPath := cleanSessionPath(session.TranscriptPath)
	if transcriptPath == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		string(session.Source), transcriptPath,
	}, "\x00")))
	return fmt.Sprintf("%x", sum[:])
}

func legacySessionTranscriptBinding(session domain.Session) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		string(session.Source), session.ExternalID,
		cleanSessionPath(session.WorkingDir), cleanSessionPath(session.TranscriptPath),
	}, "\x00")))
	return fmt.Sprintf("%x", sum[:])
}

func cleanSessionPath(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

// PruneRetention removes only ephemeral operational state. Zero durations are
// explicit opt-outs. Session rows and all durable learning/audit records remain.
func (s *Store) PruneRetention(
	ctx context.Context,
	now time.Time,
	transcriptLocators time.Duration,
	completedJobs time.Duration,
	failedJobs time.Duration,
) (RetentionPruneResult, error) {
	if now.IsZero() {
		return RetentionPruneResult{}, errors.New("store: retention time is required")
	}
	if transcriptLocators < 0 || completedJobs < 0 || failedJobs < 0 {
		return RetentionPruneResult{}, errors.New("store: retention durations cannot be negative")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RetentionPruneResult{}, fmt.Errorf("store: begin retention prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result := RetentionPruneResult{}
	if transcriptLocators > 0 {
		changed, err := tx.ExecContext(ctx, `
			UPDATE sessions SET transcript_path = ''
			WHERE transcript_path <> '' AND updated_at < ?`,
			unixNano(now.Add(-transcriptLocators)),
		)
		if err != nil {
			return RetentionPruneResult{}, fmt.Errorf("store: prune transcript locators: %w", err)
		}
		result.TranscriptLocators, err = affectedRows(changed)
		if err != nil {
			return RetentionPruneResult{}, err
		}
	}
	for _, target := range []struct {
		status    domain.JobStatus
		retention time.Duration
		count     *int
	}{
		{domain.JobCompleted, completedJobs, &result.CompletedJobs},
		{domain.JobFailed, failedJobs, &result.FailedJobs},
	} {
		if target.retention == 0 {
			continue
		}
		changed, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE status = ? AND updated_at < ?`,
			target.status, unixNano(now.Add(-target.retention)),
		)
		if err != nil {
			return RetentionPruneResult{}, fmt.Errorf("store: prune %s jobs: %w", target.status, err)
		}
		*target.count, err = affectedRows(changed)
		if err != nil {
			return RetentionPruneResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return RetentionPruneResult{}, fmt.Errorf("store: commit retention prune: %w", err)
	}
	return result, nil
}

func (s *Store) AddLearningCard(ctx context.Context, card domain.LearningCard) (bool, error) {
	return s.addLearningCard(ctx, s.db, card)
}

func (s *Store) addLearningCard(ctx context.Context, executor sqlExecutor, card domain.LearningCard) (bool, error) {
	if card.ID == "" || card.SessionRef == "" || card.SkillID == "" || card.Fingerprint == "" {
		return false, errors.New("store: card id, session, skill, and fingerprint are required")
	}
	if sanitize.ContainsSecret(card.Fingerprint) {
		return false, errors.New("store: card fingerprint contains a secret")
	}
	if !validCardKind(card.Kind) {
		return false, fmt.Errorf("store: invalid card kind %q", card.Kind)
	}
	if card.Confidence < 0 || card.Confidence > 1 {
		return false, errors.New("store: card confidence must be between 0 and 1")
	}
	// Summary and lesson originate in lower-trust transcript content. Enforce
	// the durable redaction boundary here as well as in the analyzer so direct
	// callers cannot bypass it. Fingerprints define idempotency and quorum, so
	// secret-bearing identities are rejected instead of rewritten.
	card.Summary = sanitize.Text(card.Summary)
	card.Lesson = sanitize.Text(card.Lesson)
	result, err := executor.ExecContext(ctx, `
		INSERT INTO learning_cards (
			id, session_ref, skill_id, kind, fingerprint, summary, lesson, confidence, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_ref, skill_id, fingerprint) DO NOTHING`,
		card.ID, card.SessionRef, card.SkillID, card.Kind, card.Fingerprint,
		card.Summary, card.Lesson, card.Confidence, unixNano(timeOrNow(card.CreatedAt, s.now)),
	)
	if err != nil {
		return false, fmt.Errorf("store: add learning card: %w", err)
	}
	return rowsChanged(result)
}

func (s *Store) ListLearningCards(ctx context.Context, skillID string) ([]domain.LearningCard, error) {
	query := `
		SELECT id, session_ref, skill_id, kind, fingerprint, summary, lesson, confidence, created_at
		FROM learning_cards`
	var args []any
	if skillID != "" {
		query += ` WHERE skill_id = ?`
		args = append(args, skillID)
	}
	query += ` ORDER BY created_at, id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list learning cards: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var cards []domain.LearningCard
	for rows.Next() {
		var card domain.LearningCard
		var createdAt int64
		if err := rows.Scan(
			&card.ID, &card.SessionRef, &card.SkillID, &card.Kind, &card.Fingerprint,
			&card.Summary, &card.Lesson, &card.Confidence, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan learning card: %w", err)
		}
		card.CreatedAt = fromUnixNano(createdAt)
		cards = append(cards, card)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate learning cards: %w", err)
	}
	return cards, nil
}

// RebuildClusters atomically recomputes card and distinct-session counts while
// preserving each cluster's workflow status. It returns clusters meeting the
// requested distinct-session threshold.
func (s *Store) RebuildClusters(ctx context.Context, minimumSessions int) ([]domain.Cluster, error) {
	if minimumSessions < 1 {
		return nil, errors.New("store: minimum sessions must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin cluster rebuild: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT skill_id, kind, fingerprint, COUNT(*), COUNT(DISTINCT session_ref)
		FROM learning_cards
		GROUP BY skill_id, kind, fingerprint
		ORDER BY skill_id, kind, fingerprint`)
	if err != nil {
		return nil, fmt.Errorf("store: group learning cards: %w", err)
	}
	type groupedCards struct {
		skillID, kind, fingerprint string
		cardCount, sessionCount    int
	}
	var groups []groupedCards
	for rows.Next() {
		var group groupedCards
		if err := rows.Scan(&group.skillID, &group.kind, &group.fingerprint, &group.cardCount, &group.sessionCount); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: scan grouped cards: %w", err)
		}
		groups = append(groups, group)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("store: close grouped cards: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate grouped cards: %w", err)
	}

	updatedAt := unixNano(s.now())
	for _, group := range groups {
		var summary, lesson string
		if err := tx.QueryRowContext(ctx, `
			SELECT summary, lesson
			FROM learning_cards
			WHERE skill_id = ? AND kind = ? AND fingerprint = ?
			ORDER BY created_at DESC, id DESC LIMIT 1`,
			group.skillID, group.kind, group.fingerprint,
		).Scan(&summary, &lesson); err != nil {
			return nil, fmt.Errorf("store: select cluster representative: %w", err)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO clusters (
				id, skill_id, kind, fingerprint, summary, lesson,
				card_count, session_count, status, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(skill_id, kind, fingerprint) DO UPDATE SET
				summary = excluded.summary,
				lesson = excluded.lesson,
				card_count = excluded.card_count,
				session_count = excluded.session_count,
				updated_at = excluded.updated_at`,
			clusterID(group.skillID, group.kind, group.fingerprint), group.skillID, group.kind,
			group.fingerprint, summary, lesson, group.cardCount, group.sessionCount,
			domain.ClusterOpen, updatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("store: upsert cluster: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit cluster rebuild: %w", err)
	}
	return s.ListClusters(ctx, minimumSessions)
}

func (s *Store) ListClusters(ctx context.Context, minimumSessions int) ([]domain.Cluster, error) {
	if minimumSessions < 0 {
		return nil, errors.New("store: minimum sessions cannot be negative")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, skill_id, kind, fingerprint, summary, lesson,
		       card_count, session_count, status, updated_at
		FROM clusters
		WHERE session_count >= ?
		ORDER BY updated_at DESC, id`, minimumSessions)
	if err != nil {
		return nil, fmt.Errorf("store: list clusters: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var clusters []domain.Cluster
	for rows.Next() {
		cluster, err := scanCluster(rows)
		if err != nil {
			return nil, err
		}
		clusters = append(clusters, cluster)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate clusters: %w", err)
	}
	return clusters, nil
}

func (s *Store) Cluster(ctx context.Context, id string) (domain.Cluster, error) {
	cluster, err := scanCluster(s.db.QueryRowContext(ctx, `
		SELECT id, skill_id, kind, fingerprint, summary, lesson,
		       card_count, session_count, status, updated_at
		FROM clusters WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Cluster{}, ErrNotFound
	}
	if err != nil {
		return domain.Cluster{}, err
	}
	return cluster, nil
}

func (s *Store) UpdateClusterStatus(ctx context.Context, id string, status domain.ClusterStatus) error {
	if !validClusterStatus(status) {
		return fmt.Errorf("store: invalid cluster status %q", status)
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE clusters SET status = ?, updated_at = ? WHERE id = ?`,
		status, unixNano(s.now()), id,
	)
	if err != nil {
		return fmt.Errorf("store: update cluster status: %w", err)
	}
	return expectChanged(result)
}

func (s *Store) EnqueueJob(ctx context.Context, job domain.Job) (bool, error) {
	if job.ID == "" || job.Kind == "" || job.IdempotencyKey == "" {
		return false, errors.New("store: job id, kind, and idempotency key are required")
	}
	if job.Status == "" {
		job.Status = domain.JobQueued
	}
	if job.Status != domain.JobQueued {
		return false, errors.New("store: new jobs must be queued")
	}
	now := s.now()
	availableAt := timeOrNow(job.AvailableAt, func() time.Time { return now })
	createdAt := timeOrNow(job.CreatedAt, func() time.Time { return now })
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (
			id, kind, idempotency_key, payload, status, attempts,
			available_at, leased_until, last_error, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 0, ?, NULL, '', ?, ?)
		ON CONFLICT(idempotency_key) DO NOTHING`,
		job.ID, job.Kind, job.IdempotencyKey, job.Payload, domain.JobQueued,
		unixNano(availableAt), unixNano(createdAt), unixNano(now),
	)
	if err != nil {
		return false, fmt.Errorf("store: enqueue job: %w", err)
	}
	return rowsChanged(result)
}

func (s *Store) Job(ctx context.Context, id string) (domain.Job, error) {
	if id == "" {
		return domain.Job{}, errors.New("store: job id is required")
	}
	job, err := scanJob(s.db.QueryRowContext(ctx, `
		SELECT id, kind, idempotency_key, payload, status, attempts,
		       available_at, leased_until, last_error, created_at, updated_at
		FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, ErrNotFound
	}
	if err != nil {
		return domain.Job{}, fmt.Errorf("store: get job %q: %w", id, err)
	}
	return job, nil
}

// ClaimJob leases exactly the requested job. It never substitutes another
// queued item, which keeps a filesystem event causally tied to its durable job.
// A processing job can only be reclaimed after its lease expires.
func (s *Store) ClaimJob(ctx context.Context, id string, lease time.Duration) (domain.Job, bool, error) {
	if id == "" || lease <= 0 {
		return domain.Job{}, false, errors.New("store: job id and lease must be positive")
	}
	now := s.now()
	row := s.db.QueryRowContext(ctx, `
		UPDATE jobs
		SET status = ?, attempts = attempts + 1, leased_until = ?, updated_at = ?
		WHERE id = ?
		  AND (
			(status = ? AND available_at <= ?)
			OR (status = ? AND leased_until <= ?)
		  )
		RETURNING id, kind, idempotency_key, payload, status, attempts,
		          available_at, leased_until, last_error, created_at, updated_at`,
		domain.JobProcessing, unixNano(now.Add(lease)), unixNano(now), id,
		domain.JobQueued, unixNano(now), domain.JobProcessing, unixNano(now),
	)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, false, nil
	}
	if err != nil {
		return domain.Job{}, false, fmt.Errorf("store: claim job %q: %w", id, err)
	}
	return job, true, nil
}

// ClaimJobs leases queued work and expired leases in one SQLite statement.
func (s *Store) ClaimJobs(ctx context.Context, limit int, lease time.Duration) ([]domain.Job, error) {
	if limit < 1 || lease <= 0 {
		return nil, errors.New("store: claim limit and lease must be positive")
	}
	now := s.now()
	rows, err := s.db.QueryContext(ctx, `
		UPDATE jobs
		SET status = ?, attempts = attempts + 1, leased_until = ?, updated_at = ?
		WHERE id IN (
			SELECT id FROM jobs
			WHERE (status = ? AND available_at <= ?)
			   OR (status = ? AND leased_until <= ?)
			ORDER BY available_at, created_at, id
			LIMIT ?
		)
		RETURNING id, kind, idempotency_key, payload, status, attempts,
		          available_at, leased_until, last_error, created_at, updated_at`,
		domain.JobProcessing, unixNano(now.Add(lease)), unixNano(now),
		domain.JobQueued, unixNano(now), domain.JobProcessing, unixNano(now), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: claim jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var jobs []domain.Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate claimed jobs: %w", err)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].AvailableAt.Equal(jobs[j].AvailableAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].AvailableAt.Before(jobs[j].AvailableAt)
	})
	return jobs, nil
}

func (s *Store) CompleteJob(ctx context.Context, id string, fencingToken int) error {
	if id == "" || fencingToken <= 0 {
		return errors.New("store: job id and fencing token are required")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, leased_until = NULL, last_error = '', updated_at = ?
		WHERE id = ? AND status = ? AND attempts = ?`,
		domain.JobCompleted, unixNano(s.now()), id, domain.JobProcessing, fencingToken,
	)
	if err != nil {
		return fmt.Errorf("store: complete job: %w", err)
	}
	return expectChanged(result)
}

// CommitIngestJob persists one normalized session and its learning cards, then
// completes the exact lease generation in a single write transaction. Taking
// the SQLite write lock through the guarded UPDATE before any learning write
// prevents a stale worker from committing after a newer worker reclaimed the
// job.
func (s *Store) CommitIngestJob(
	ctx context.Context,
	id string,
	fencingToken int,
	session domain.Session,
	cards []domain.LearningCard,
) (int, error) {
	if id == "" || fencingToken <= 0 {
		return 0, errors.New("store: job id and fencing token are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin fenced ingest commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var guardedID string
	err = tx.QueryRowContext(ctx, `
		UPDATE jobs SET updated_at = updated_at
		WHERE id = ? AND status = ? AND attempts = ?
		RETURNING id`,
		id, domain.JobProcessing, fencingToken,
	).Scan(&guardedID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: fence ingest lease: %w", err)
	}
	if _, err := s.recordSession(ctx, tx, session); err != nil {
		return 0, err
	}
	createdCards := 0
	for _, card := range cards {
		created, err := s.addLearningCard(ctx, tx, card)
		if err != nil {
			return 0, err
		}
		if created {
			createdCards++
		}
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE jobs SET status = ?, leased_until = NULL, last_error = '', updated_at = ?
		WHERE id = ? AND status = ? AND attempts = ?`,
		domain.JobCompleted, unixNano(s.now()), id, domain.JobProcessing, fencingToken,
	)
	if err != nil {
		return 0, fmt.Errorf("store: complete fenced ingest job: %w", err)
	}
	if err := expectChanged(result); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit fenced ingest job: %w", err)
	}
	return createdCards, nil
}

func (s *Store) RetryJob(ctx context.Context, id string, fencingToken int, message string, availableAt time.Time, terminal bool) error {
	if id == "" || fencingToken <= 0 {
		return errors.New("store: job id and fencing token are required")
	}
	status := domain.JobQueued
	if terminal {
		status = domain.JobFailed
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET status = ?, available_at = ?, leased_until = NULL, last_error = ?, updated_at = ?
		WHERE id = ? AND status = ? AND attempts = ?`,
		status, unixNano(timeOrNow(availableAt, s.now)), message, unixNano(s.now()), id, domain.JobProcessing, fencingToken,
	)
	if err != nil {
		return fmt.Errorf("store: retry job: %w", err)
	}
	return expectChanged(result)
}

func (s *Store) SaveProposal(ctx context.Context, proposal domain.Proposal) error {
	if proposal.ID == "" || proposal.ClusterID == "" || proposal.SkillID == "" || proposal.RepositoryPath == "" {
		return errors.New("store: proposal id, cluster, skill, and repository path are required")
	}
	if !validProposalStatus(proposal.Status) {
		return fmt.Errorf("store: invalid proposal status %q", proposal.Status)
	}
	if proposal.BaseCommit != "" || proposal.CandidateCommit != "" {
		if proposal.BaseCommit == "" || proposal.CandidateCommit == "" || proposal.Fingerprint == "" ||
			proposal.Lesson == "" || !validCardKind(proposal.CardKind) {
			return errors.New("store: prepared proposal requires exact commits and candidate safety metadata")
		}
		if (proposal.CardKind == domain.CardCorrection || proposal.CardKind == domain.CardFailure) && !proposal.RequiresHumanApproval {
			return errors.New("store: correction and failure proposals require human approval")
		}
	}
	now := s.now()
	createdAt := timeOrNow(proposal.CreatedAt, func() time.Time { return now })
	updatedAt := timeOrNow(proposal.UpdatedAt, func() time.Time { return now })
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO proposals (
			id, cluster_id, skill_id, fingerprint, lesson, card_kind, requires_human_approval,
			status, repository_path, worktree_path,
			branch, base_commit, candidate_commit, baseline_score, candidate_score,
			previous_commit, promoted_commit, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			fingerprint = excluded.fingerprint,
			lesson = excluded.lesson,
			card_kind = excluded.card_kind,
			requires_human_approval = excluded.requires_human_approval,
			status = excluded.status,
			worktree_path = excluded.worktree_path,
			branch = excluded.branch,
			base_commit = excluded.base_commit,
			candidate_commit = excluded.candidate_commit,
			baseline_score = excluded.baseline_score,
			candidate_score = excluded.candidate_score,
			previous_commit = excluded.previous_commit,
			promoted_commit = excluded.promoted_commit,
			updated_at = excluded.updated_at
		WHERE proposals.cluster_id = excluded.cluster_id
		  AND proposals.skill_id = excluded.skill_id
		  AND proposals.repository_path = excluded.repository_path
		  AND (
			(proposals.base_commit = '' AND proposals.candidate_commit = '')
			OR (
				proposals.base_commit = excluded.base_commit
				AND proposals.candidate_commit = excluded.candidate_commit
				AND proposals.fingerprint = excluded.fingerprint
				AND proposals.lesson = excluded.lesson
				AND proposals.card_kind = excluded.card_kind
				AND proposals.requires_human_approval = excluded.requires_human_approval
			)
		  )`,
		proposal.ID, proposal.ClusterID, proposal.SkillID, proposal.Fingerprint,
		proposal.Lesson, proposal.CardKind, boolInt(proposal.RequiresHumanApproval), proposal.Status,
		proposal.RepositoryPath, proposal.WorktreePath, proposal.Branch,
		proposal.BaseCommit, proposal.CandidateCommit, proposal.BaselineScore,
		proposal.CandidateScore, proposal.PreviousCommit, proposal.PromotedCommit,
		unixNano(createdAt), unixNano(updatedAt),
	)
	if err != nil {
		return fmt.Errorf("store: save proposal: %w", err)
	}
	if err := expectChanged(result); err != nil {
		return fmt.Errorf("store: proposal identity or candidate metadata drifted: %w", err)
	}
	return nil
}

// ReserveProposal atomically claims an open cluster for one pending proposal.
// A deterministic proposal ID makes this safe across multiple local processes.
func (s *Store) ReserveProposal(ctx context.Context, proposal domain.Proposal) (bool, error) {
	if proposal.ID == "" || proposal.ClusterID == "" || proposal.SkillID == "" || proposal.RepositoryPath == "" {
		return false, errors.New("store: proposal id, cluster, skill, and repository path are required")
	}
	if proposal.Status != domain.ProposalPending {
		return false, errors.New("store: reserved proposal must be pending")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: begin proposal reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	claimed, err := tx.ExecContext(ctx, `
		UPDATE clusters SET status = ?, updated_at = ?
		WHERE id = ? AND skill_id = ? AND status = ?`,
		domain.ClusterProposed, unixNano(s.now()), proposal.ClusterID, proposal.SkillID, domain.ClusterOpen,
	)
	if err != nil {
		return false, fmt.Errorf("store: claim proposal cluster: %w", err)
	}
	changed, err := rowsChanged(claimed)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	now := s.now()
	createdAt := timeOrNow(proposal.CreatedAt, func() time.Time { return now })
	inserted, err := tx.ExecContext(ctx, `
		INSERT INTO proposals (
			id, cluster_id, skill_id, status, repository_path, worktree_path,
			branch, base_commit, candidate_commit, baseline_score, candidate_score,
			previous_commit, promoted_commit, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, '', '', '', '', 0, 0, '', '', ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		proposal.ID, proposal.ClusterID, proposal.SkillID, domain.ProposalPending,
		proposal.RepositoryPath, unixNano(createdAt), unixNano(now),
	)
	if err != nil {
		return false, fmt.Errorf("store: reserve proposal: %w", err)
	}
	created, err := rowsChanged(inserted)
	if err != nil {
		return false, err
	}
	if !created {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit proposal reservation: %w", err)
	}
	return true, nil
}

func (s *Store) AbandonProposal(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin abandon proposal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var clusterID string
	if err := tx.QueryRowContext(ctx,
		`SELECT cluster_id FROM proposals WHERE id = ? AND status = ?`, id, domain.ProposalPending,
	).Scan(&clusterID); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("store: find reserved proposal: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM proposals WHERE id = ? AND status = ?`, id, domain.ProposalPending); err != nil {
		return fmt.Errorf("store: delete reserved proposal: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE clusters SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		domain.ClusterOpen, unixNano(s.now()), clusterID, domain.ClusterProposed,
	); err != nil {
		return fmt.Errorf("store: reopen proposal cluster: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit abandon proposal: %w", err)
	}
	return nil
}

// AbandonProposalIfUnchanged abandons only the exact empty reservation
// observed by the caller. A concurrently refreshed or populated proposal is
// preserved, preventing stale daemon snapshots from deleting live work.
func (s *Store) AbandonProposalIfUnchanged(ctx context.Context, id string, observedUpdatedAt time.Time) (bool, error) {
	if id == "" || observedUpdatedAt.IsZero() {
		return false, errors.New("store: proposal id and observed update time are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: begin compare-and-swap abandon proposal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var clusterID string
	err = tx.QueryRowContext(ctx, `
		DELETE FROM proposals
		WHERE id = ? AND status = ? AND base_commit = '' AND candidate_commit = '' AND updated_at = ?
		RETURNING cluster_id`,
		id, domain.ProposalPending, unixNano(observedUpdatedAt),
	).Scan(&clusterID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: compare-and-swap abandon proposal: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE clusters SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		domain.ClusterOpen, unixNano(s.now()), clusterID, domain.ClusterProposed,
	); err != nil {
		return false, fmt.Errorf("store: reopen abandoned proposal cluster: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit compare-and-swap abandon proposal: %w", err)
	}
	return true, nil
}

func (s *Store) Proposal(ctx context.Context, id string) (domain.Proposal, error) {
	row := s.db.QueryRowContext(ctx, proposalSelect+` WHERE id = ?`, id)
	proposal, err := scanProposal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Proposal{}, ErrNotFound
	}
	if err != nil {
		return domain.Proposal{}, err
	}
	return proposal, nil
}

func (s *Store) ProposalForCluster(ctx context.Context, clusterID string) (domain.Proposal, error) {
	proposal, err := scanProposal(s.db.QueryRowContext(ctx,
		proposalSelect+` WHERE cluster_id = ? ORDER BY created_at DESC, id LIMIT 1`, clusterID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Proposal{}, ErrNotFound
	}
	if err != nil {
		return domain.Proposal{}, err
	}
	return proposal, nil
}

func (s *Store) ListProposals(ctx context.Context, status domain.ProposalStatus) ([]domain.Proposal, error) {
	query := proposalSelect
	var args []any
	if status != "" {
		if !validProposalStatus(status) {
			return nil, fmt.Errorf("store: invalid proposal status %q", status)
		}
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list proposals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var proposals []domain.Proposal
	for rows.Next() {
		proposal, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		proposals = append(proposals, proposal)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate proposals: %w", err)
	}
	return proposals, nil
}

func (s *Store) UpdateProposalStatus(ctx context.Context, id string, status domain.ProposalStatus) error {
	if !validProposalStatus(status) {
		return fmt.Errorf("store: invalid proposal status %q", status)
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE proposals SET status = ?, updated_at = ? WHERE id = ?`,
		status, unixNano(s.now()), id,
	)
	if err != nil {
		return fmt.Errorf("store: update proposal status: %w", err)
	}
	return expectChanged(result)
}

func (s *Store) DeleteProposal(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM proposals WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete proposal: %w", err)
	}
	return expectChanged(result)
}

func (s *Store) RecordEvaluationResult(ctx context.Context, result domain.EvaluationResult) (bool, error) {
	if result.ID == "" || result.ProposalID == "" || !validEvaluationVariant(result.Variant) {
		return false, errors.New("store: evaluation id, proposal, and valid variant are required")
	}
	inserted, err := s.db.ExecContext(ctx, `
		INSERT INTO evaluation_results (
			id, proposal_id, variant, passed, score, duration_ns, details, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		result.ID, result.ProposalID, result.Variant, boolInt(result.Passed), result.Score,
		int64(result.Duration), result.Details, unixNano(timeOrNow(result.CreatedAt, s.now)),
	)
	if err != nil {
		return false, fmt.Errorf("store: record evaluation result: %w", err)
	}
	return rowsChanged(inserted)
}

func (s *Store) ListEvaluationResults(ctx context.Context, proposalID string) ([]domain.EvaluationResult, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, proposal_id, variant, passed, score, duration_ns, details, created_at
		FROM evaluation_results WHERE proposal_id = ? ORDER BY created_at, id`, proposalID)
	if err != nil {
		return nil, fmt.Errorf("store: list evaluation results: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []domain.EvaluationResult
	for rows.Next() {
		var result domain.EvaluationResult
		var passed int
		var duration, createdAt int64
		if err := rows.Scan(
			&result.ID, &result.ProposalID, &result.Variant, &passed, &result.Score,
			&duration, &result.Details, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan evaluation result: %w", err)
		}
		result.Passed = passed != 0
		result.Duration = time.Duration(duration)
		result.CreatedAt = fromUnixNano(createdAt)
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate evaluation results: %w", err)
	}
	return results, nil
}

// CompleteEvaluation persists the exact baseline/candidate pair and transitions
// the matching pending proposal in one transaction.
func (s *Store) CompleteEvaluation(ctx context.Context, proposal domain.Proposal, results ...domain.EvaluationResult) error {
	if proposal.ID == "" || proposal.BaseCommit == "" || proposal.CandidateCommit == "" || len(results) != 2 {
		return errors.New("store: complete evaluation requires a proposal, exact commits, and two results")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin evaluation completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	seen := map[domain.EvaluationVariant]bool{}
	for _, result := range results {
		if result.ID == "" || result.ProposalID != proposal.ID || !validEvaluationVariant(result.Variant) || seen[result.Variant] {
			return errors.New("store: evaluation results must contain unique baseline and candidate variants")
		}
		seen[result.Variant] = true
		_, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_results (
				id, proposal_id, variant, passed, score, duration_ns, details, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				passed = excluded.passed,
				score = excluded.score,
				duration_ns = excluded.duration_ns,
				details = excluded.details,
				created_at = excluded.created_at`,
			result.ID, result.ProposalID, result.Variant, boolInt(result.Passed), result.Score,
			int64(result.Duration), result.Details, unixNano(timeOrNow(result.CreatedAt, s.now)),
		)
		if err != nil {
			return fmt.Errorf("store: persist evaluation result: %w", err)
		}
	}
	if !seen[domain.EvaluationBaseline] || !seen[domain.EvaluationCandidate] {
		return errors.New("store: evaluation requires baseline and candidate variants")
	}
	updated, err := tx.ExecContext(ctx, `
		UPDATE proposals SET status = ?, baseline_score = ?, candidate_score = ?, updated_at = ?
		WHERE id = ? AND base_commit = ? AND candidate_commit = ?
		  AND status IN (?, ?)`,
		domain.ProposalEvaluated, proposal.BaselineScore, proposal.CandidateScore, unixNano(s.now()),
		proposal.ID, proposal.BaseCommit, proposal.CandidateCommit,
		domain.ProposalPending, domain.ProposalEvaluated,
	)
	if err != nil {
		return fmt.Errorf("store: mark proposal evaluated: %w", err)
	}
	if err := expectChanged(updated); err != nil {
		return fmt.Errorf("store: evaluation proposal drifted: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit evaluation: %w", err)
	}
	return nil
}

func (s *Store) ApproveProposal(ctx context.Context, id, baseCommit, candidateCommit, actor, details string) error {
	if actor == "" {
		return errors.New("store: approval actor is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin proposal approval: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var status domain.ProposalStatus
	var storedBase, storedCandidate string
	if err := tx.QueryRowContext(ctx,
		`SELECT status, base_commit, candidate_commit FROM proposals WHERE id = ?`, id,
	).Scan(&status, &storedBase, &storedCandidate); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("store: inspect proposal approval: %w", err)
	}
	if storedBase != baseCommit || storedCandidate != candidateCommit {
		return errors.New("store: proposal approval commit drift")
	}
	if status == domain.ProposalApproved {
		return nil
	}
	if status != domain.ProposalEvaluated {
		return fmt.Errorf("store: cannot approve proposal in status %q", status)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE proposals SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		domain.ProposalApproved, unixNano(s.now()), id, domain.ProposalEvaluated,
	); err != nil {
		return fmt.Errorf("store: approve proposal: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log (action, entity_type, entity_id, actor, details, created_at)
		VALUES ('proposal.approved', 'proposal', ?, ?, ?, ?)`,
		id, actor, details, unixNano(s.now()),
	); err != nil {
		return fmt.Errorf("store: audit proposal approval: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit proposal approval: %w", err)
	}
	return nil
}

func (s *Store) RejectProposal(ctx context.Context, id, actor, details string) error {
	if actor == "" {
		return errors.New("store: rejection actor is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin proposal rejection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var clusterID string
	var status domain.ProposalStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT cluster_id, status FROM proposals WHERE id = ?`, id,
	).Scan(&clusterID, &status); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("store: inspect proposal rejection: %w", err)
	}
	if status == domain.ProposalRejected {
		return nil
	}
	if status != domain.ProposalPending && status != domain.ProposalEvaluated && status != domain.ProposalApproved {
		return fmt.Errorf("store: cannot reject proposal in status %q", status)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE proposals SET status = ?, updated_at = ? WHERE id = ?`,
		domain.ProposalRejected, unixNano(s.now()), id,
	); err != nil {
		return fmt.Errorf("store: reject proposal: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE clusters SET status = ?, updated_at = ? WHERE id = ?`,
		domain.ClusterResolved, unixNano(s.now()), clusterID,
	); err != nil {
		return fmt.Errorf("store: resolve rejected proposal cluster: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log (action, entity_type, entity_id, actor, details, created_at)
		VALUES ('proposal.rejected', 'proposal', ?, ?, ?, ?)`,
		id, actor, details, unixNano(s.now()),
	); err != nil {
		return fmt.Errorf("store: audit proposal rejection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit proposal rejection: %w", err)
	}
	return nil
}

// RecordPromotion atomically deactivates the previous release for a skill and
// marks the corresponding proposal promoted.
func (s *Store) RecordPromotion(ctx context.Context, promotion domain.Promotion) error {
	return s.RecordPromotionDecision(ctx, promotion, "system", "")
}

func (s *Store) RecordPromotionDecision(ctx context.Context, promotion domain.Promotion, actor, details string) error {
	if promotion.ID == "" || promotion.ProposalID == "" || promotion.SkillID == "" || promotion.PromotedCommit == "" {
		return errors.New("store: promotion id, proposal, skill, and promoted commit are required")
	}
	if actor == "" {
		return errors.New("store: promotion actor is required")
	}
	if promotion.MonitorStatus == "" {
		promotion.MonitorStatus = domain.MonitorPending
	}
	if !validMonitorStatus(promotion.MonitorStatus) {
		return fmt.Errorf("store: invalid monitor status %q", promotion.MonitorStatus)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin promotion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, existingErr := scanPromotion(tx.QueryRowContext(ctx, promotionSelect+` WHERE id = ?`, promotion.ID))
	if existingErr == nil {
		if existing.ProposalID != promotion.ProposalID || existing.SkillID != promotion.SkillID ||
			existing.PreviousCommit != promotion.PreviousCommit || existing.PromotedCommit != promotion.PromotedCommit || !existing.Active {
			return errors.New("store: promotion id already has different payload or is inactive")
		}
		return nil
	}
	if !errors.Is(existingErr, sql.ErrNoRows) {
		return existingErr
	}
	var proposalSkill, baseCommit, candidateCommit string
	var proposalStatus domain.ProposalStatus
	if err := tx.QueryRowContext(ctx, `
		SELECT skill_id, status, base_commit, candidate_commit
		FROM proposals WHERE id = ?`, promotion.ProposalID,
	).Scan(&proposalSkill, &proposalStatus, &baseCommit, &candidateCommit); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("store: inspect promotion proposal: %w", err)
	}
	if proposalSkill != promotion.SkillID || baseCommit != promotion.PreviousCommit || candidateCommit != promotion.PromotedCommit {
		return errors.New("store: promotion does not match exact proposal revisions")
	}
	if proposalStatus != domain.ProposalEvaluated && proposalStatus != domain.ProposalApproved {
		return fmt.Errorf("store: cannot promote proposal in status %q", proposalStatus)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE promotions SET active = 0 WHERE skill_id = ?`, promotion.SkillID); err != nil {
		return fmt.Errorf("store: deactivate previous promotion: %w", err)
	}
	promotedAt := timeOrNow(promotion.PromotedAt, s.now)
	lastMonitoredAt := nullableTime(promotion.LastMonitoredAt)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO promotions (
			id, proposal_id, skill_id, previous_commit, promoted_commit,
			active, monitor_status, promoted_at, last_monitored_at
		) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		promotion.ID, promotion.ProposalID, promotion.SkillID, promotion.PreviousCommit,
		promotion.PromotedCommit, promotion.MonitorStatus, unixNano(promotedAt), lastMonitoredAt,
	); err != nil {
		return fmt.Errorf("store: insert promotion: %w", err)
	}
	updated, err := tx.ExecContext(ctx, `
		UPDATE proposals SET status = ?, promoted_commit = ?, previous_commit = ?, updated_at = ?
		WHERE id = ? AND skill_id = ? AND base_commit = ? AND candidate_commit = ?
		  AND status IN (?, ?)`,
		domain.ProposalPromoted, promotion.PromotedCommit, promotion.PreviousCommit,
		unixNano(s.now()), promotion.ProposalID, promotion.SkillID,
		promotion.PreviousCommit, promotion.PromotedCommit,
		domain.ProposalEvaluated, domain.ProposalApproved,
	)
	if err != nil {
		return fmt.Errorf("store: mark proposal promoted: %w", err)
	}
	if err := expectChanged(updated); err != nil {
		return fmt.Errorf("store: promotion proposal drifted: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log (action, entity_type, entity_id, actor, details, created_at)
		VALUES ('promotion.created', 'promotion', ?, ?, ?, ?)`,
		promotion.ID, actor, details, unixNano(s.now()),
	); err != nil {
		return fmt.Errorf("store: audit promotion: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit promotion: %w", err)
	}
	return nil
}

func (s *Store) ActivePromotion(ctx context.Context, skillID string) (domain.Promotion, error) {
	row := s.db.QueryRowContext(ctx, promotionSelect+` WHERE skill_id = ? AND active = 1`, skillID)
	promotion, err := scanPromotion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Promotion{}, ErrNotFound
	}
	if err != nil {
		return domain.Promotion{}, err
	}
	return promotion, nil
}

func (s *Store) Promotion(ctx context.Context, id string) (domain.Promotion, error) {
	row := s.db.QueryRowContext(ctx, promotionSelect+` WHERE id = ?`, id)
	promotion, err := scanPromotion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Promotion{}, ErrNotFound
	}
	if err != nil {
		return domain.Promotion{}, err
	}
	return promotion, nil
}

func (s *Store) ListPromotions(ctx context.Context, activeOnly bool) ([]domain.Promotion, error) {
	query := promotionSelect
	if activeOnly {
		query += ` WHERE active = 1`
	}
	query += ` ORDER BY promoted_at DESC, id`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: list promotions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var promotions []domain.Promotion
	for rows.Next() {
		promotion, err := scanPromotion(rows)
		if err != nil {
			return nil, err
		}
		promotions = append(promotions, promotion)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate promotions: %w", err)
	}
	return promotions, nil
}

func (s *Store) UpdatePromotionMonitor(ctx context.Context, id string, status domain.MonitorStatus) error {
	if !validMonitorStatus(status) {
		return fmt.Errorf("store: invalid monitor status %q", status)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE promotions SET monitor_status = ?, last_monitored_at = ? WHERE id = ? AND active = 1`,
		status, unixNano(s.now()), id,
	)
	if err != nil {
		return fmt.Errorf("store: update promotion monitor: %w", err)
	}
	return expectChanged(result)
}

// RecordRollback records the rollback and deactivates the promoted revision in
// the same transaction. Filesystem activation remains the release module's job.
func (s *Store) RecordRollback(ctx context.Context, rollback domain.Rollback) error {
	if rollback.ID == "" || rollback.PromotionID == "" || rollback.FromCommit == "" || rollback.ToCommit == "" || rollback.Reason == "" || rollback.Actor == "" {
		return errors.New("store: rollback id, promotion, commits, reason, and actor are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin rollback: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	createdAt := timeOrNow(rollback.CreatedAt, s.now)
	inserted, err := tx.ExecContext(ctx, `
		INSERT INTO rollbacks (
			id, promotion_id, from_commit, to_commit, reason, actor, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		rollback.ID, rollback.PromotionID, rollback.FromCommit, rollback.ToCommit,
		rollback.Reason, rollback.Actor, unixNano(createdAt),
	)
	if err != nil {
		return fmt.Errorf("store: insert rollback: %w", err)
	}
	created, err := rowsChanged(inserted)
	if err != nil {
		return err
	}
	if !created {
		var existing domain.Rollback
		var existingCreatedAt int64
		if err := tx.QueryRowContext(ctx, `
			SELECT id, promotion_id, from_commit, to_commit, reason, actor, created_at
			FROM rollbacks WHERE id = ?`, rollback.ID,
		).Scan(
			&existing.ID, &existing.PromotionID, &existing.FromCommit, &existing.ToCommit,
			&existing.Reason, &existing.Actor, &existingCreatedAt,
		); err != nil {
			return fmt.Errorf("store: inspect existing rollback: %w", err)
		}
		if existing.PromotionID != rollback.PromotionID || existing.FromCommit != rollback.FromCommit ||
			existing.ToCommit != rollback.ToCommit || existing.Reason != rollback.Reason || existing.Actor != rollback.Actor {
			return errors.New("store: rollback id already has different payload")
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit existing rollback: %w", err)
		}
		return nil
	}
	var promotedCommit, previousCommit string
	if err := tx.QueryRowContext(ctx, `
		SELECT promoted_commit, previous_commit
		FROM promotions WHERE id = ? AND active = 1`, rollback.PromotionID,
	).Scan(&promotedCommit, &previousCommit); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("store: inspect rollback promotion: %w", err)
	}
	if promotedCommit != rollback.FromCommit || previousCommit != rollback.ToCommit {
		return errors.New("store: rollback does not match active promotion revisions")
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE promotions SET active = 0, monitor_status = ? WHERE id = ? AND active = 1`,
		domain.MonitorRolledBack, rollback.PromotionID,
	)
	if err != nil {
		return fmt.Errorf("store: deactivate rolled back promotion: %w", err)
	}
	changed, err := rowsChanged(result)
	if err != nil {
		return err
	}
	if !changed {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE proposals SET status = ?, updated_at = ?
		WHERE id = (SELECT proposal_id FROM promotions WHERE id = ?)`,
		domain.ProposalRolledBack, unixNano(s.now()), rollback.PromotionID,
	); err != nil {
		return fmt.Errorf("store: mark proposal rolled back: %w", err)
	}
	auditDetails, err := json.Marshal(map[string]string{
		"from_commit": rollback.FromCommit,
		"to_commit":   rollback.ToCommit,
		"reason":      rollback.Reason,
	})
	if err != nil {
		return fmt.Errorf("store: encode rollback audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log (action, entity_type, entity_id, actor, details, created_at)
		VALUES ('promotion.rolled_back', 'promotion', ?, ?, ?, ?)`,
		rollback.PromotionID, rollback.Actor, string(auditDetails), unixNano(createdAt),
	); err != nil {
		return fmt.Errorf("store: audit rollback: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit rollback: %w", err)
	}
	return nil
}

func (s *Store) ListRollbacks(ctx context.Context, promotionID string) ([]domain.Rollback, error) {
	query := `
		SELECT id, promotion_id, from_commit, to_commit, reason, actor, created_at
		FROM rollbacks`
	var args []any
	if promotionID != "" {
		query += ` WHERE promotion_id = ?`
		args = append(args, promotionID)
	}
	query += ` ORDER BY created_at DESC, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list rollbacks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var rollbacks []domain.Rollback
	for rows.Next() {
		var rollback domain.Rollback
		var createdAt int64
		if err := rows.Scan(
			&rollback.ID, &rollback.PromotionID, &rollback.FromCommit, &rollback.ToCommit,
			&rollback.Reason, &rollback.Actor, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan rollback: %w", err)
		}
		rollback.CreatedAt = fromUnixNano(createdAt)
		rollbacks = append(rollbacks, rollback)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate rollbacks: %w", err)
	}
	return rollbacks, nil
}

func (s *Store) AppendAudit(ctx context.Context, entry domain.AuditEntry) (domain.AuditEntry, error) {
	if entry.Action == "" || entry.EntityType == "" || entry.EntityID == "" || entry.Actor == "" {
		return domain.AuditEntry{}, errors.New("store: audit action, entity type, entity id, and actor are required")
	}
	entry.CreatedAt = timeOrNow(entry.CreatedAt, s.now)
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_log (action, entity_type, entity_id, actor, details, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		entry.Action, entry.EntityType, entry.EntityID, entry.Actor, entry.Details, unixNano(entry.CreatedAt),
	)
	if err != nil {
		return domain.AuditEntry{}, fmt.Errorf("store: append audit: %w", err)
	}
	entry.ID, err = result.LastInsertId()
	if err != nil {
		return domain.AuditEntry{}, fmt.Errorf("store: read audit id: %w", err)
	}
	return entry, nil
}

func (s *Store) ListAudit(ctx context.Context, entityType, entityID string) ([]domain.AuditEntry, error) {
	query := `SELECT id, action, entity_type, entity_id, actor, details, created_at FROM audit_log`
	var args []any
	if entityType != "" || entityID != "" {
		query += ` WHERE entity_type = ? AND entity_id = ?`
		args = append(args, entityType, entityID)
	}
	query += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []domain.AuditEntry
	for rows.Next() {
		var entry domain.AuditEntry
		var createdAt int64
		if err := rows.Scan(
			&entry.ID, &entry.Action, &entry.EntityType, &entry.EntityID,
			&entry.Actor, &entry.Details, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan audit entry: %w", err)
		}
		entry.CreatedAt = fromUnixNano(createdAt)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate audit entries: %w", err)
	}
	return entries, nil
}

func (s *Store) Counts(ctx context.Context, eligibleMinimumSessions int) (Counts, error) {
	if eligibleMinimumSessions < 1 {
		return Counts{}, errors.New("store: eligible minimum sessions must be positive")
	}
	counts := Counts{
		Jobs:      make(map[domain.JobStatus]int),
		Proposals: make(map[domain.ProposalStatus]int),
	}
	queries := []struct {
		query string
		args  []any
		dest  *int
	}{
		{`SELECT COUNT(*) FROM sessions`, nil, &counts.Sessions},
		{`SELECT COUNT(*) FROM skills`, nil, &counts.Skills},
		{`SELECT COUNT(*) FROM learning_cards`, nil, &counts.Cards},
		{`SELECT COUNT(*) FROM clusters`, nil, &counts.Clusters},
		{`SELECT COUNT(*) FROM clusters WHERE session_count >= ?`, []any{eligibleMinimumSessions}, &counts.EligibleClusters},
		{`SELECT COUNT(*) FROM promotions WHERE active = 1`, nil, &counts.ActivePromotions},
		{`SELECT COUNT(*) FROM rollbacks`, nil, &counts.Rollbacks},
	}
	for _, item := range queries {
		if err := s.db.QueryRowContext(ctx, item.query, item.args...).Scan(item.dest); err != nil {
			return Counts{}, fmt.Errorf("store: count state: %w", err)
		}
	}
	if err := countStatuses(ctx, s.db, `SELECT status, COUNT(*) FROM jobs GROUP BY status`, func(status string, count int) {
		counts.Jobs[domain.JobStatus(status)] = count
	}); err != nil {
		return Counts{}, err
	}
	if err := countStatuses(ctx, s.db, `SELECT status, COUNT(*) FROM proposals GROUP BY status`, func(status string, count int) {
		counts.Proposals[domain.ProposalStatus(status)] = count
	}); err != nil {
		return Counts{}, err
	}
	return counts, nil
}

const proposalSelect = `
	SELECT id, cluster_id, skill_id, fingerprint, lesson, card_kind, requires_human_approval,
	       status, repository_path, worktree_path,
	       branch, base_commit, candidate_commit, baseline_score, candidate_score,
	       previous_commit, promoted_commit, created_at, updated_at
	FROM proposals`

const promotionSelect = `
	SELECT id, proposal_id, skill_id, previous_commit, promoted_commit,
	       active, monitor_status, promoted_at, last_monitored_at
	FROM promotions`

type scanner interface {
	Scan(dest ...any) error
}

func scanCluster(row scanner) (domain.Cluster, error) {
	var cluster domain.Cluster
	var updatedAt int64
	if err := row.Scan(
		&cluster.ID, &cluster.SkillID, &cluster.Kind, &cluster.Fingerprint,
		&cluster.Summary, &cluster.Lesson, &cluster.CardCount, &cluster.SessionCount,
		&cluster.Status, &updatedAt,
	); err != nil {
		return domain.Cluster{}, fmt.Errorf("store: scan cluster: %w", err)
	}
	cluster.UpdatedAt = fromUnixNano(updatedAt)
	return cluster, nil
}

func scanProposal(row scanner) (domain.Proposal, error) {
	var proposal domain.Proposal
	var requiresHumanApproval int
	var createdAt, updatedAt int64
	if err := row.Scan(
		&proposal.ID, &proposal.ClusterID, &proposal.SkillID, &proposal.Fingerprint,
		&proposal.Lesson, &proposal.CardKind, &requiresHumanApproval, &proposal.Status,
		&proposal.RepositoryPath, &proposal.WorktreePath, &proposal.Branch,
		&proposal.BaseCommit, &proposal.CandidateCommit, &proposal.BaselineScore,
		&proposal.CandidateScore, &proposal.PreviousCommit, &proposal.PromotedCommit,
		&createdAt, &updatedAt,
	); err != nil {
		return domain.Proposal{}, fmt.Errorf("store: scan proposal: %w", err)
	}
	proposal.CreatedAt = fromUnixNano(createdAt)
	proposal.UpdatedAt = fromUnixNano(updatedAt)
	proposal.RequiresHumanApproval = requiresHumanApproval != 0
	return proposal, nil
}

func scanPromotion(row scanner) (domain.Promotion, error) {
	var promotion domain.Promotion
	var active int
	var promotedAt int64
	var lastMonitoredAt sql.NullInt64
	if err := row.Scan(
		&promotion.ID, &promotion.ProposalID, &promotion.SkillID,
		&promotion.PreviousCommit, &promotion.PromotedCommit, &active,
		&promotion.MonitorStatus, &promotedAt, &lastMonitoredAt,
	); err != nil {
		return domain.Promotion{}, fmt.Errorf("store: scan promotion: %w", err)
	}
	promotion.Active = active != 0
	promotion.PromotedAt = fromUnixNano(promotedAt)
	if lastMonitoredAt.Valid {
		promotion.LastMonitoredAt = fromUnixNano(lastMonitoredAt.Int64)
	}
	return promotion, nil
}

func scanJob(row scanner) (domain.Job, error) {
	var job domain.Job
	var availableAt, createdAt, updatedAt int64
	var leasedUntil sql.NullInt64
	if err := row.Scan(
		&job.ID, &job.Kind, &job.IdempotencyKey, &job.Payload, &job.Status,
		&job.Attempts, &availableAt, &leasedUntil, &job.LastError, &createdAt, &updatedAt,
	); err != nil {
		return domain.Job{}, fmt.Errorf("store: scan job: %w", err)
	}
	job.AvailableAt = fromUnixNano(availableAt)
	job.FencingToken = job.Attempts
	if leasedUntil.Valid {
		job.LeasedUntil = fromUnixNano(leasedUntil.Int64)
	}
	job.CreatedAt = fromUnixNano(createdAt)
	job.UpdatedAt = fromUnixNano(updatedAt)
	return job, nil
}

func countStatuses(ctx context.Context, db *sql.DB, query string, set func(string, int)) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("store: count statuses: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return fmt.Errorf("store: scan status count: %w", err)
		}
		set(status, count)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate status counts: %w", err)
	}
	return nil
}

func clusterID(skillID, kind, fingerprint string) string {
	sum := sha256.Sum256([]byte(skillID + "\x00" + kind + "\x00" + fingerprint))
	return fmt.Sprintf("cluster-%x", sum[:12])
}

func rowsChanged(result sql.Result) (bool, error) {
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: read affected rows: %w", err)
	}
	return count > 0, nil
}

func affectedRows(result sql.Result) (int, error) {
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: read affected rows: %w", err)
	}
	return int(count), nil
}

func expectChanged(result sql.Result) error {
	changed, err := rowsChanged(result)
	if err != nil {
		return err
	}
	if !changed {
		return ErrNotFound
	}
	return nil
}

func unixNano(value time.Time) int64 {
	return value.UTC().UnixNano()
}

func fromUnixNano(value int64) time.Time {
	return time.Unix(0, value).UTC()
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return unixNano(value)
}

func timeOrNow(value time.Time, now func() time.Time) time.Time {
	if value.IsZero() {
		return now().UTC()
	}
	return value.UTC()
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func validCardKind(kind domain.CardKind) bool {
	return kind == domain.CardCorrection || kind == domain.CardFailure || kind == domain.CardValidation
}

func validClusterStatus(status domain.ClusterStatus) bool {
	return status == domain.ClusterOpen || status == domain.ClusterProposed || status == domain.ClusterResolved
}

func validProposalStatus(status domain.ProposalStatus) bool {
	switch status {
	case domain.ProposalPending, domain.ProposalEvaluated, domain.ProposalApproved,
		domain.ProposalPromoted, domain.ProposalRejected, domain.ProposalRolledBack:
		return true
	default:
		return false
	}
}

func validEvaluationVariant(variant domain.EvaluationVariant) bool {
	return variant == domain.EvaluationBaseline || variant == domain.EvaluationCandidate
}

func validMonitorStatus(status domain.MonitorStatus) bool {
	return status == domain.MonitorPending || status == domain.MonitorHealthy ||
		status == domain.MonitorRegressing || status == domain.MonitorRolledBack
}
