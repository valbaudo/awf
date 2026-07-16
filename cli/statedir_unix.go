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

func stateDirInfo(path string) (statePathMetadata, error) {
	info, err := os.Stat(path)
	if err != nil {
		return statePathMetadata{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return statePathMetadata{Mode: info.Mode(), UnixPermissions: true}, nil
	}
	mode := (info.Mode() &^ fs.ModePerm) | fs.FileMode(uint32(stat.Mode)&uint32(fs.ModePerm))
	return unixStatePathMetadata(mode, int(stat.Uid), int(stat.Gid)), nil
}

func syscallENOTDIR() error { return syscall.ENOTDIR }
