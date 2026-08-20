package improvement

import "golang.org/x/sys/unix"

func stateDevice(stat *unix.Stat_t) uint64 {
	return uint64(stat.Dev)
}
