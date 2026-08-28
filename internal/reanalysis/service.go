// Package reanalysis rebuilds learning artifacts from durable session metadata
// and provider-owned transcripts.
package reanalysis

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/learning"
	"github.com/flemzord/skillloop/internal/store"
	"github.com/flemzord/skillloop/internal/transcript"
)

const maximumSearchEntries = 100_000

type Options struct {
	DryRun          bool
	MinimumSessions int
}

type Issue struct {
	SessionRef string `json:"session_ref"`
	Reason     string `json:"reason"`
}

type Result struct {
	DryRun            bool    `json:"dry_run"`
	Sessions          int     `json:"sessions"`
	Resolved          int     `json:"resolved"`
	Missing           int     `json:"missing"`
	Failed            int     `json:"failed"`
	SessionsWithCards int     `json:"sessions_with_cards"`
	CardsFound        int     `json:"cards_found"`
	CardsNew          int     `json:"cards_new"`
	CardsCreated      int     `json:"cards_created"`
	Clusters          int     `json:"clusters"`
	EligibleClusters  int     `json:"eligible_clusters"`
	Issues            []Issue `json:"issues,omitempty"`
}

type Service struct {
	Store      *store.Store
	Normalizer transcript.Normalizer
	Analyzer   learning.Analyzer
}

func (service Service) Run(ctx context.Context, options Options) (Result, error) {
	result := Result{DryRun: options.DryRun}
	if service.Store == nil {
		return result, errors.New("reanalysis: store is required")
	}
	if options.MinimumSessions < 1 {
		return result, errors.New("reanalysis: minimum sessions must be positive")
	}

	sessions, err := service.Store.ListSessions(ctx)
	if err != nil {
		return result, err
	}
	result.Sessions = len(sessions)
	skills, err := service.Store.ListSkills(ctx)
	if err != nil {
		return result, err
	}
	existing, err := service.Store.ListLearningCards(ctx, "")
	if err != nil {
		return result, err
	}

	known := make(map[string]struct{}, len(existing))
	allCards := append([]domain.LearningCard(nil), existing...)
	for _, card := range existing {
		known[cardIdentity(card)] = struct{}{}
	}
	var newCards []domain.LearningCard
	for _, metadata := range sessions {
		session, resolveErr := service.resolve(ctx, metadata)
		if resolveErr != nil {
			issue := Issue{SessionRef: metadata.Reference, Reason: resolveErr.Error()}
			result.Issues = append(result.Issues, issue)
			if errors.Is(resolveErr, fs.ErrNotExist) {
				result.Missing++
			} else {
				result.Failed++
			}
			continue
		}
		result.Resolved++
		cards := service.Analyzer.Analyze(session, skills)
		result.CardsFound += len(cards)
		if len(cards) > 0 {
			result.SessionsWithCards++
		}
		for _, card := range cards {
			identity := cardIdentity(card)
			if _, exists := known[identity]; exists {
				continue
			}
			known[identity] = struct{}{}
			newCards = append(newCards, card)
			allCards = append(allCards, card)
		}
	}
	result.CardsNew = len(newCards)
	result.Clusters, result.EligibleClusters = projectedClusters(allCards, options.MinimumSessions)

	if options.DryRun {
		return result, nil
	}
	created, err := service.Store.AddLearningCards(ctx, newCards)
	if err != nil {
		return result, err
	}
	result.CardsCreated = created
	if _, err := service.Store.RebuildClusters(ctx, options.MinimumSessions); err != nil {
		return result, err
	}
	clusters, err := service.Store.ListClusters(ctx, 0)
	if err != nil {
		return result, err
	}
	eligible, err := service.Store.ListClusters(ctx, options.MinimumSessions)
	if err != nil {
		return result, err
	}
	result.Clusters = len(clusters)
	result.EligibleClusters = len(eligible)
	return result, nil
}

func (service Service) resolve(ctx context.Context, metadata domain.Session) (domain.Session, error) {
	if metadata.TranscriptPath != "" {
		if _, err := os.Lstat(metadata.TranscriptPath); err == nil {
			return service.normalizeAndVerify(ctx, metadata, metadata.TranscriptPath)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return domain.Session{}, fmt.Errorf("inspect stored transcript: %w", err)
		}
	}

	roots, err := providerRoots(service.Normalizer, metadata.Source)
	if err != nil {
		return domain.Session{}, err
	}
	var matches []domain.Session
	var candidateErrors int
	entries := 0
	for _, root := range roots {
		walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if errors.Is(walkErr, fs.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			entries++
			if entries > maximumSearchEntries {
				return errors.New("provider transcript search exceeded its entry limit")
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.Contains(entry.Name(), metadata.ExternalID) {
				return nil
			}
			session, normalizeErr := service.normalizeAndVerify(ctx, metadata, path)
			if normalizeErr != nil {
				candidateErrors++
				return nil
			}
			matches = append(matches, session)
			return nil
		})
		if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
			return domain.Session{}, fmt.Errorf("search provider transcripts: %w", walkErr)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return domain.Session{}, errors.New("multiple provider transcripts match the stored session identity")
	}
	if candidateErrors > 0 {
		return domain.Session{}, errors.New("matching transcript candidates failed identity validation")
	}
	return domain.Session{}, fmt.Errorf("provider transcript not found: %w", fs.ErrNotExist)
}

func (service Service) normalizeAndVerify(ctx context.Context, metadata domain.Session, path string) (domain.Session, error) {
	session, err := service.Normalizer.Normalize(ctx, domain.HookEvent{
		SchemaVersion:  1,
		Source:         metadata.Source,
		SessionID:      metadata.ExternalID,
		TurnID:         metadata.TurnID,
		WorkingDir:     metadata.WorkingDir,
		TranscriptPath: path,
	})
	if err != nil {
		return domain.Session{}, err
	}
	if session.Reference != metadata.Reference || session.Source != metadata.Source || session.ExternalID != metadata.ExternalID {
		return domain.Session{}, errors.New("normalized transcript identity does not match stored session")
	}
	if filepath.Clean(session.WorkingDir) != filepath.Clean(metadata.WorkingDir) {
		return domain.Session{}, errors.New("normalized transcript working directory does not match stored session")
	}
	return session, nil
}

func providerRoots(normalizer transcript.Normalizer, source domain.Source) ([]string, error) {
	if roots := normalizer.AllowedRoots[source]; len(roots) > 0 {
		return roots, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve transcript root: %w", err)
	}
	switch source {
	case domain.SourceCodex:
		root := os.Getenv("CODEX_HOME")
		if root == "" {
			root = filepath.Join(home, ".codex")
		}
		return []string{filepath.Join(root, "sessions"), filepath.Join(root, "archived_sessions")}, nil
	case domain.SourceClaude:
		root := os.Getenv("CLAUDE_CONFIG_DIR")
		if root == "" {
			root = filepath.Join(home, ".claude")
		}
		return []string{filepath.Join(root, "projects")}, nil
	default:
		return nil, fmt.Errorf("unsupported source %q", source)
	}
}

func cardIdentity(card domain.LearningCard) string {
	return card.SessionRef + "\x00" + card.SkillID + "\x00" + card.Fingerprint
}

func projectedClusters(cards []domain.LearningCard, minimumSessions int) (int, int) {
	sessionsByCluster := make(map[string]map[string]struct{})
	for _, card := range cards {
		key := card.SkillID + "\x00" + string(card.Kind) + "\x00" + card.Fingerprint
		if sessionsByCluster[key] == nil {
			sessionsByCluster[key] = make(map[string]struct{})
		}
		sessionsByCluster[key][card.SessionRef] = struct{}{}
	}
	eligible := 0
	for _, sessions := range sessionsByCluster {
		if len(sessions) >= minimumSessions {
			eligible++
		}
	}
	return len(sessionsByCluster), eligible
}

func SortIssues(issues []Issue) {
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].SessionRef == issues[j].SessionRef {
			return issues[i].Reason < issues[j].Reason
		}
		return issues[i].SessionRef < issues[j].SessionRef
	})
}
