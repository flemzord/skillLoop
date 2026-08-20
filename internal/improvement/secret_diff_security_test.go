package improvement

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPrepareRejectsSharedSCMSecretFamiliesBeforeCreatingGitState(t *testing.T) {
	for name, credential := range map[string]string{
		"github fine grained": "github_pat_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUV",
		"gitlab":              "glpat-abcdefghijklmnopqrstuvwx",
	} {
		t.Run(name, func(t *testing.T) {
			repository, skill := newTestRepository(t)
			before, err := git(context.Background(), repository, "show-ref")
			if err != nil {
				t.Fatalf("list refs before prepare: %v", err)
			}
			cluster := testCluster(skill.ID)
			cluster.Lesson = "Use " + credential + " for Git access."
			_, err = (Service{StateDir: t.TempDir()}).Prepare(context.Background(), skill, cluster)
			if !errors.Is(err, ErrUnsafeChange) {
				t.Fatalf("prepare error = %v, want ErrUnsafeChange", err)
			}
			after, err := git(context.Background(), repository, "show-ref")
			if err != nil {
				t.Fatalf("list refs after prepare: %v", err)
			}
			if after != before {
				t.Fatalf("secret rejection created Git refs:\nbefore: %s\nafter: %s", before, after)
			}
		})
	}
}

func TestLineCountCountsCraftedHeaderLikeContentInsideHunks(t *testing.T) {
	var diff strings.Builder
	diff.WriteString("diff --git a/SKILL.md b/SKILL.md\n")
	diff.WriteString("--- a/SKILL.md\n")
	diff.WriteString("+++ b/SKILL.md\n")
	diff.WriteString("@@ -1,0 +1,31 @@\n")
	for index := range 31 {
		_, _ = fmt.Fprintf(&diff, "+++crafted-%02d\n", index)
	}

	count, err := lineCount(diff.String())
	if err != nil {
		t.Fatalf("count crafted diff: %v", err)
	}
	if count != 31 {
		t.Fatalf("changed line count = %d, want 31", count)
	}
	if err := validateDiff(diff.String()); !errors.Is(err, ErrDiffLimit) {
		t.Fatalf("validate crafted diff = %v, want ErrDiffLimit", err)
	}
}

func TestLineCountIgnoresOnlyStructuralDiffRecords(t *testing.T) {
	diff := "diff --git a/SKILL.md b/SKILL.md\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/SKILL.md\n" +
		"+++ b/SKILL.md\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new\n"
	count, err := lineCount(diff)
	if err != nil || count != 2 {
		t.Fatalf("lineCount() = %d, %v; want 2, nil", count, err)
	}
	if err := validateDiff(diff); err != nil {
		t.Fatalf("small valid diff rejected: %v", err)
	}
}
