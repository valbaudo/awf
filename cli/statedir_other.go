//go:build !unix

package cli

import (
	"errors"
	"io/fs"
	"os"
)

func currentStateIdentity() stateIdentity { return stateIdentity{UID: -1, GID: -1} }

func stateDirInfo(path string) (statePathMetadata, error) {
	info, err := os.Stat(path)
	if err != nil {
		return statePathMetadata{}, err
	}
	return statePathMetadata{Mode: info.Mode()}, nil
}

func syscallENOTDIR() error { return errors.New("not a directory") }
