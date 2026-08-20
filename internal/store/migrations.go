package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/flemzord/skillloop/internal/domain"
)

const schemaVersion = 4

const schemaV1 = `
CREATE TABLE skills (
	id TEXT PRIMARY KEY CHECK (length(id) > 0),
	name TEXT NOT NULL CHECK (length(name) > 0),
	repository_path TEXT NOT NULL CHECK (length(repository_path) > 0),
	instruction_path TEXT NOT NULL CHECK (length(instruction_path) > 0),
	enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
	created_at INTEGER NOT NULL,
	UNIQUE(repository_path, instruction_path)
);

CREATE TABLE sessions (
	reference TEXT PRIMARY KEY CHECK (length(reference) > 0),
	source TEXT NOT NULL CHECK (source IN ('codex', 'claude')),
	external_id TEXT NOT NULL CHECK (length(external_id) > 0),
	turn_id TEXT NOT NULL DEFAULT '',
	working_dir TEXT NOT NULL DEFAULT '',
	transcript_path TEXT NOT NULL DEFAULT '',
	outcome TEXT NOT NULL DEFAULT 'unknown' CHECK (outcome IN ('unknown', 'succeeded', 'failed')),
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(source, external_id)
);

CREATE TABLE learning_cards (
	id TEXT PRIMARY KEY CHECK (length(id) > 0),
	session_ref TEXT NOT NULL REFERENCES sessions(reference) ON DELETE CASCADE,
	skill_id TEXT NOT NULL REFERENCES skills(id) ON DELETE RESTRICT,
	kind TEXT NOT NULL CHECK (kind IN ('correction', 'failure', 'validation')),
	fingerprint TEXT NOT NULL CHECK (length(fingerprint) > 0),
	summary TEXT NOT NULL,
	lesson TEXT NOT NULL,
	confidence REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
	created_at INTEGER NOT NULL,
	UNIQUE(session_ref, skill_id, fingerprint)
);

CREATE TABLE clusters (
	id TEXT PRIMARY KEY CHECK (length(id) > 0),
	skill_id TEXT NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('correction', 'failure', 'validation')),
	fingerprint TEXT NOT NULL,
	summary TEXT NOT NULL,
	lesson TEXT NOT NULL,
	card_count INTEGER NOT NULL CHECK (card_count >= 0),
	session_count INTEGER NOT NULL CHECK (session_count >= 0),
	status TEXT NOT NULL CHECK (status IN ('open', 'proposed', 'resolved')),
	updated_at INTEGER NOT NULL,
	UNIQUE(skill_id, kind, fingerprint)
);

CREATE TABLE jobs (
	id TEXT PRIMARY KEY CHECK (length(id) > 0),
	kind TEXT NOT NULL CHECK (length(kind) > 0),
	idempotency_key TEXT NOT NULL UNIQUE CHECK (length(idempotency_key) > 0),
	payload TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('queued', 'processing', 'completed', 'failed')),
	attempts INTEGER NOT NULL CHECK (attempts >= 0),
	available_at INTEGER NOT NULL,
	leased_until INTEGER,
	last_error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);

CREATE TABLE proposals (
	id TEXT PRIMARY KEY CHECK (length(id) > 0),
	cluster_id TEXT NOT NULL REFERENCES clusters(id) ON DELETE RESTRICT,
	skill_id TEXT NOT NULL REFERENCES skills(id) ON DELETE RESTRICT,
	status TEXT NOT NULL CHECK (status IN ('pending', 'evaluated', 'approved', 'promoted', 'rejected', 'rolled_back')),
	repository_path TEXT NOT NULL,
	worktree_path TEXT NOT NULL,
	branch TEXT NOT NULL,
	base_commit TEXT NOT NULL,
	candidate_commit TEXT NOT NULL,
	baseline_score REAL NOT NULL,
	candidate_score REAL NOT NULL,
	previous_commit TEXT NOT NULL,
	promoted_commit TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);

CREATE TABLE evaluation_results (
	id TEXT PRIMARY KEY CHECK (length(id) > 0),
	proposal_id TEXT NOT NULL REFERENCES proposals(id) ON DELETE CASCADE,
	variant TEXT NOT NULL CHECK (variant IN ('baseline', 'candidate')),
	passed INTEGER NOT NULL CHECK (passed IN (0, 1)),
	score REAL NOT NULL,
	duration_ns INTEGER NOT NULL CHECK (duration_ns >= 0),
	details TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	UNIQUE(proposal_id, variant)
);

CREATE TABLE promotions (
	id TEXT PRIMARY KEY CHECK (length(id) > 0),
	proposal_id TEXT NOT NULL UNIQUE REFERENCES proposals(id) ON DELETE RESTRICT,
	skill_id TEXT NOT NULL REFERENCES skills(id) ON DELETE RESTRICT,
	previous_commit TEXT NOT NULL,
	promoted_commit TEXT NOT NULL CHECK (length(promoted_commit) > 0),
	active INTEGER NOT NULL CHECK (active IN (0, 1)),
	monitor_status TEXT NOT NULL CHECK (monitor_status IN ('pending', 'healthy', 'regressing', 'rolled_back')),
	promoted_at INTEGER NOT NULL,
	last_monitored_at INTEGER
);

CREATE UNIQUE INDEX promotions_one_active_per_skill
	ON promotions(skill_id) WHERE active = 1;

CREATE TABLE rollbacks (
	id TEXT PRIMARY KEY CHECK (length(id) > 0),
	promotion_id TEXT NOT NULL REFERENCES promotions(id) ON DELETE RESTRICT,
	from_commit TEXT NOT NULL CHECK (length(from_commit) > 0),
	to_commit TEXT NOT NULL CHECK (length(to_commit) > 0),
	reason TEXT NOT NULL CHECK (length(reason) > 0),
	actor TEXT NOT NULL CHECK (length(actor) > 0),
	created_at INTEGER NOT NULL
);

CREATE TABLE audit_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	action TEXT NOT NULL CHECK (length(action) > 0),
	entity_type TEXT NOT NULL CHECK (length(entity_type) > 0),
	entity_id TEXT NOT NULL CHECK (length(entity_id) > 0),
	actor TEXT NOT NULL CHECK (length(actor) > 0),
	details TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE INDEX learning_cards_cluster_idx
	ON learning_cards(skill_id, kind, fingerprint, session_ref);
CREATE INDEX clusters_eligibility_idx
	ON clusters(status, session_count, updated_at);
CREATE INDEX jobs_available_idx
	ON jobs(status, available_at, leased_until);
CREATE INDEX proposals_status_idx
	ON proposals(status, updated_at);
CREATE UNIQUE INDEX proposals_one_live_per_cluster
	ON proposals(cluster_id)
	WHERE status IN ('pending', 'evaluated', 'approved', 'promoted');
CREATE INDEX audit_entity_idx
	ON audit_log(entity_type, entity_id, id);
`

const schemaV2 = `
ALTER TABLE proposals ADD COLUMN fingerprint TEXT NOT NULL DEFAULT '';
ALTER TABLE proposals ADD COLUMN lesson TEXT NOT NULL DEFAULT '';
ALTER TABLE proposals ADD COLUMN card_kind TEXT NOT NULL DEFAULT ''
	CHECK (card_kind IN ('', 'correction', 'failure', 'validation'));
ALTER TABLE proposals ADD COLUMN requires_human_approval INTEGER NOT NULL DEFAULT 1
	CHECK (requires_human_approval IN (0, 1));
`

const schemaV3 = `
ALTER TABLE sessions ADD COLUMN transcript_binding TEXT NOT NULL DEFAULT '';
`

const schemaV4 = `
CREATE UNIQUE INDEX sessions_transcript_binding_unique
	ON sessions(transcript_binding) WHERE transcript_binding <> '';
`

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("store: create migration registry: %w", err)
	}

	var version int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version)
	if err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("store: database schema %d is newer than supported schema %d", version, schemaVersion)
	}
	if version == 0 {
		if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
			return fmt.Errorf("store: apply schema v1: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			1, unixNano(s.now()),
		); err != nil {
			return fmt.Errorf("store: record schema v1: %w", err)
		}
		version = 1
	}
	if version == 1 {
		if _, err := tx.ExecContext(ctx, schemaV2); err != nil {
			return fmt.Errorf("store: apply schema v2: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			2, unixNano(s.now()),
		); err != nil {
			return fmt.Errorf("store: record schema v2: %w", err)
		}
		version = 2
	}
	if version == 2 {
		if _, err := tx.ExecContext(ctx, schemaV3); err != nil {
			return fmt.Errorf("store: apply schema v3: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			3, unixNano(s.now()),
		); err != nil {
			return fmt.Errorf("store: record schema v3: %w", err)
		}
		version = 3
	}
	if version == 3 {
		if err := backfillSessionTranscriptBindings(ctx, tx); err != nil {
			return fmt.Errorf("store: backfill schema v4 transcript bindings: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaV4); err != nil {
			return fmt.Errorf("store: apply schema v4: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			4, unixNano(s.now()),
		); err != nil {
			return fmt.Errorf("store: record schema v4: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration: %w", err)
	}
	return nil
}

func backfillSessionTranscriptBindings(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT reference, source, external_id, working_dir, transcript_path, transcript_binding
		FROM sessions`)
	if err != nil {
		return err
	}
	type update struct {
		reference string
		binding   string
	}
	updates := make([]update, 0)
	for rows.Next() {
		var reference, source, externalID, workingDir, transcriptPath, storedBinding string
		if err := rows.Scan(&reference, &source, &externalID, &workingDir, &transcriptPath, &storedBinding); err != nil {
			_ = rows.Close()
			return err
		}
		session := domain.Session{
			Source: domain.Source(source), ExternalID: externalID,
			WorkingDir: workingDir, TranscriptPath: transcriptPath,
		}
		if transcriptPath == "" {
			if storedBinding != "" && storedBinding != legacySessionTranscriptBinding(session) {
				_ = rows.Close()
				return fmt.Errorf("session %q has an unverifiable retained transcript binding", reference)
			}
			if storedBinding != "" {
				updates = append(updates, update{reference: reference})
			}
			continue
		}
		binding := sessionTranscriptBinding(session)
		if binding != storedBinding {
			updates = append(updates, update{reference: reference, binding: binding})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET transcript_binding = ? WHERE reference = ?`,
			update.binding, update.reference,
		); err != nil {
			return err
		}
	}
	return nil
}
