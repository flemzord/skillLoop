//go:build darwin

package improvement

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDarwinSingleProcessEvaluatorIsContained(t *testing.T) {
	service := Service{Runner: Runner{
		Argv:    []string{"/bin/sh", "-c", "printf single-process-ok"},
		Timeout: time.Second,
	}}
	result, err := service.runOne(context.Background(), t.TempDir(), strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Output != "single-process-ok" || !result.ContainmentVerified {
		t.Fatalf("single-process evaluator result = %#v", result)
	}
}

func TestDarwinMissingEvaluatorIsInfrastructureFailure(t *testing.T) {
	service := Service{Runner: Runner{
		Argv:    []string{"skillloop-evaluator-that-does-not-exist"},
		Timeout: time.Second,
	}}
	_, err := service.runOne(context.Background(), t.TempDir(), strings.Repeat("b", 40))
	if err == nil || !strings.Contains(err.Error(), "is unavailable") {
		t.Fatalf("missing evaluator error = %v, want infrastructure failure", err)
	}
}

func TestKillEvaluatorProcessDoesNotSignalReapedPID(t *testing.T) {
	command := exec.Command("/usr/bin/true")
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	killCalls := 0
	terminator := &processGroupTerminator{
		command: command,
		kill: func(command *exec.Cmd) error {
			killCalls++
			return killEvaluatorProcess(command)
		},
		killAfterWait: false,
	}
	if err := terminator.terminateAfterWait(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill reaped evaluator = %v, want process done", err)
	}
	if killCalls != 0 {
		t.Fatalf("post-Wait cleanup signaled a reaped PID %d times", killCalls)
	}
}
