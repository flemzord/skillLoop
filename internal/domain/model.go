package domain

import "time"

type Source string

const (
	SourceCodex  Source = "codex"
	SourceClaude Source = "claude"
)

func (source Source) Valid() bool {
	return source == SourceCodex || source == SourceClaude
}

type AutonomyMode string

const (
	ModeObserve   AutonomyMode = "observe"
	ModePropose   AutonomyMode = "propose"
	ModeAutopilot AutonomyMode = "autopilot"
)

func (mode AutonomyMode) Valid() bool {
	return mode == ModeObserve || mode == ModePropose || mode == ModeAutopilot
}

type HookEvent struct {
	SchemaVersion       int       `json:"schema_version"`
	ID                  string    `json:"event_id"`
	Source              Source    `json:"provider"`
	SessionID           string    `json:"session_id"`
	TurnID              string    `json:"turn_id,omitempty"`
	PromptID            string    `json:"prompt_id,omitempty"`
	TranscriptPath      string    `json:"transcript_path,omitempty"`
	WorkingDir          string    `json:"cwd"`
	HookEventName       string    `json:"event"`
	PermissionMode      string    `json:"permission_mode,omitempty"`
	Model               string    `json:"model,omitempty"`
	Effort              string    `json:"effort,omitempty"`
	StopHookActive      *bool     `json:"stop_hook_active,omitempty"`
	Reason              string    `json:"reason,omitempty"`
	BackgroundTaskCount *int      `json:"background_task_count,omitempty"`
	SessionCronCount    *int      `json:"session_cron_count,omitempty"`
	CapturedAt          time.Time `json:"captured_at"`
}

type Message struct {
	Role       string
	Text       string
	ToolName   string
	ToolCallID string
	ToolResult bool
	Failed     bool
}

type Session struct {
	Reference      string
	Source         Source
	ExternalID     string
	TurnID         string
	WorkingDir     string
	TranscriptPath string
	Messages       []Message
}

type Skill struct {
	ID              string
	Name            string
	RepositoryPath  string
	InstructionPath string
	Enabled         bool
	CreatedAt       time.Time
}

type CardKind string

const (
	CardCorrection CardKind = "correction"
	CardFailure    CardKind = "failure"
	CardValidation CardKind = "validation"
)

type LearningCard struct {
	ID          string
	SessionRef  string
	SkillID     string
	Kind        CardKind
	Fingerprint string
	Summary     string
	Lesson      string
	Confidence  float64
	CreatedAt   time.Time
}

type ClusterStatus string

const (
	ClusterOpen     ClusterStatus = "open"
	ClusterProposed ClusterStatus = "proposed"
	ClusterResolved ClusterStatus = "resolved"
)

type Cluster struct {
	ID           string
	SkillID      string
	Kind         CardKind
	Fingerprint  string
	Summary      string
	Lesson       string
	CardCount    int
	SessionCount int
	Status       ClusterStatus
	UpdatedAt    time.Time
}

type ProposalStatus string

const (
	ProposalPending    ProposalStatus = "pending"
	ProposalEvaluated  ProposalStatus = "evaluated"
	ProposalApproved   ProposalStatus = "approved"
	ProposalPromoted   ProposalStatus = "promoted"
	ProposalRejected   ProposalStatus = "rejected"
	ProposalRolledBack ProposalStatus = "rolled_back"
)

type Proposal struct {
	ID              string
	ClusterID       string
	SkillID         string
	Status          ProposalStatus
	RepositoryPath  string
	WorktreePath    string
	Branch          string
	BaseCommit      string
	CandidateCommit string
	BaselineScore   float64
	CandidateScore  float64
	PreviousCommit  string
	PromotedCommit  string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type EvaluationVariant string

const (
	EvaluationBaseline  EvaluationVariant = "baseline"
	EvaluationCandidate EvaluationVariant = "candidate"
)

type EvaluationResult struct {
	ID         string
	ProposalID string
	Variant    EvaluationVariant
	Passed     bool
	Score      float64
	Duration   time.Duration
	Details    string
	CreatedAt  time.Time
}

type MonitorStatus string

const (
	MonitorPending    MonitorStatus = "pending"
	MonitorHealthy    MonitorStatus = "healthy"
	MonitorRegressing MonitorStatus = "regressing"
	MonitorRolledBack MonitorStatus = "rolled_back"
)

type Promotion struct {
	ID              string
	ProposalID      string
	SkillID         string
	PreviousCommit  string
	PromotedCommit  string
	Active          bool
	MonitorStatus   MonitorStatus
	PromotedAt      time.Time
	LastMonitoredAt time.Time
}

type Rollback struct {
	ID          string
	PromotionID string
	FromCommit  string
	ToCommit    string
	Reason      string
	Actor       string
	CreatedAt   time.Time
}

type JobStatus string

const (
	JobQueued     JobStatus = "queued"
	JobProcessing JobStatus = "processing"
	JobCompleted  JobStatus = "completed"
	JobFailed     JobStatus = "failed"
)

type Job struct {
	ID             string
	Kind           string
	IdempotencyKey string
	Payload        string
	Status         JobStatus
	Attempts       int
	AvailableAt    time.Time
	LeasedUntil    time.Time
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type AuditEntry struct {
	ID         int64
	Action     string
	EntityType string
	EntityID   string
	Actor      string
	Details    string
	CreatedAt  time.Time
}
