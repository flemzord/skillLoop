//go:build linux

package improvement

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestProcessGroupContainmentFilterRejectsX32Syscalls(t *testing.T) {
	architecture, err := linuxAuditArchitecture()
	if err != nil {
		t.Fatal(err)
	}
	filter := processGroupContainmentFilter(architecture)
	errno := uint32(unix.EPERM) & unix.SECCOMP_RET_DATA

	for name, test := range map[string]struct {
		architecture uint32
		syscall      uint32
		want         uint32
	}{
		"native regular syscall": {architecture, uint32(unix.SYS_GETPID), unix.SECCOMP_RET_ALLOW},
		"native setsid":          {architecture, uint32(unix.SYS_SETSID), unix.SECCOMP_RET_ERRNO | errno},
		"native setpgid":         {architecture, uint32(unix.SYS_SETPGID), unix.SECCOMP_RET_ERRNO | errno},
		"x32 regular syscall":    {architecture, linuxX32SyscallBit | uint32(unix.SYS_GETPID), unix.SECCOMP_RET_KILL_PROCESS},
		"x32 setsid":             {architecture, linuxX32SyscallBit | uint32(unix.SYS_SETSID), unix.SECCOMP_RET_KILL_PROCESS},
		"wrong architecture":     {architecture ^ 1, uint32(unix.SYS_GETPID), unix.SECCOMP_RET_KILL_PROCESS},
	} {
		t.Run(name, func(t *testing.T) {
			if got := evaluateSeccompFilter(t, filter, test.architecture, test.syscall); got != test.want {
				t.Fatalf("filter result = %#x, want %#x", got, test.want)
			}
		})
	}
}

func evaluateSeccompFilter(t *testing.T, filter []unix.SockFilter, architecture, syscall uint32) uint32 {
	t.Helper()
	var accumulator uint32
	for pc := 0; pc < len(filter); pc++ {
		instruction := filter[pc]
		switch instruction.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			switch instruction.K {
			case 0:
				accumulator = syscall
			case 4:
				accumulator = architecture
			default:
				t.Fatalf("unexpected seccomp_data offset %d", instruction.K)
			}
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if accumulator == instruction.K {
				pc += int(instruction.Jt)
			} else {
				pc += int(instruction.Jf)
			}
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			if accumulator&instruction.K != 0 {
				pc += int(instruction.Jt)
			} else {
				pc += int(instruction.Jf)
			}
		case unix.BPF_RET | unix.BPF_K:
			return instruction.K
		default:
			t.Fatalf("unexpected BPF instruction %#x", instruction.Code)
		}
	}
	t.Fatal("seccomp filter returned no decision")
	return 0
}
