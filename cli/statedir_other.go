//go:build !unix

package cli

import (
	"errors"
	"io/fs"
	"os"
)

func currentStateIdentity() stateIdentity { return stateIdentity{UID: -1, GID: -1} }

func stateDirInfo(path string) (ownerUID int, ownerKnown bool, mode fs.FileMode, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false, 0, err
	}
	return 0, false, info.Mode(), nil
}

func syscallENOTDIR() error { return errors.New("not a directory") }
