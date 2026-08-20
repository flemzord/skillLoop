//go:build linux

package improvement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	linuxEvaluatorBootstrap = "SKILLLOOP_INTERNAL_EVALUATOR_BOOTSTRAP"
	maxBootstrapArgvBytes   = 64 * 1024
	linuxX32SyscallBit      = uint32(0x40000000)
)

// init is a narrow self-exec bootstrap. Go's exec API cannot install a seccomp
// filter between fork and exec, so the child re-enters the current binary,
// installs the inherited filter, and replaces itself with the configured
// evaluator. The marker is removed before that final exec.
func init() {
	encoded := os.Getenv(linuxEvaluatorBootstrap)
	if encoded == "" {
		return
	}
	if len(encoded) > maxBootstrapArgvBytes {
		bootstrapFailure(errors.New("evaluator argv exceeds bootstrap limit"))
	}
	var argv []string
	if err := json.Unmarshal([]byte(encoded), &argv); err != nil || len(argv) == 0 || argv[0] == "" {
		bootstrapFailure(errors.New("invalid evaluator bootstrap argv"))
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		bootstrapFailure(fmt.Errorf("resolve evaluator executable: %w", err))
	}
	runtime.LockOSThread()
	if err := installProcessGroupContainment(); err != nil {
		bootstrapFailure(fmt.Errorf("install evaluator containment: %w", err))
	}
	ready, err := signalEvaluatorContainment()
	if err != nil {
		bootstrapFailure(fmt.Errorf("signal evaluator containment: %w", err))
	}
	environment := withoutEnvironmentKey(os.Environ(), linuxEvaluatorBootstrap)
	if err := syscall.Exec(path, argv, environment); err != nil {
		signalEvaluatorExecFailure(ready)
		bootstrapFailure(fmt.Errorf("exec evaluator: %w", err))
	}
}

func containedEvaluatorCommand(ctx context.Context, argv []string) (*exec.Cmd, *processGroupTerminator, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, nil, errors.New("external runner executable is empty")
	}
	runner, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, nil, fmt.Errorf("external runner executable %q is unavailable: %w", argv[0], err)
	}
	exactArgv := append([]string{runner}, argv[1:]...)
	encoded, err := json.Marshal(exactArgv)
	if err != nil || len(encoded) > maxBootstrapArgvBytes {
		return nil, nil, errors.New("external runner argv exceeds containment limit")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve evaluator bootstrap: %w", err)
	}
	command := exec.CommandContext(ctx, executable)
	command.Env = append(withoutEnvironmentKey(os.Environ(), linuxEvaluatorBootstrap), linuxEvaluatorBootstrap+"="+string(encoded))
	terminator, err := configureContainedProcess(command)
	if err != nil {
		return nil, nil, err
	}
	return command, terminator, nil
}

func installProcessGroupContainment() error {
	architecture, err := linuxAuditArchitecture()
	if err != nil {
		return err
	}
	filter := processGroupContainmentFilter(architecture)
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	return unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0)
}

func processGroupContainmentFilter(architecture uint32) []unix.SockFilter {
	errno := uint32(unix.EPERM) & unix.SECCOMP_RET_DATA
	return []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: architecture},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		// x32 uses AUDIT_ARCH_X86_64 while setting bit 30 on the syscall
		// number. Reject it before comparing native syscall numbers so it
		// cannot bypass the setsid/setpgid policy.
		{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jf: 1, K: linuxX32SyscallBit},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(unix.SYS_SETSID)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | errno},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(unix.SYS_SETPGID)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | errno},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
}

func linuxAuditArchitecture() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return 0xc000003e, nil
	case "arm64":
		return 0xc00000b7, nil
	default:
		return 0, fmt.Errorf("unsupported Linux evaluator architecture %q", runtime.GOARCH)
	}
}
