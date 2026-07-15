//go:build unix

package cli

import (
	"io/fs"
	"os"
	"syscall"
)

func currentStateIdentity() stateIdentity {
	return stateIdentity{UID: os.Geteuid(), GID: os.Getegid()}
}

func stateDirInfo(path string) (ownerUID int, ownerKnown bool, mode fs.FileMode, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false, info.Mode(), nil
	}
	return int(stat.Uid), true, info.Mode(), nil
}

func syscallENOTDIR() error { return syscall.ENOTDIR }
