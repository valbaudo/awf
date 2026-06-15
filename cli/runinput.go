package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// resolveRunInput resolves the run-input bytes from the mutually-exclusive --input
// (inline JSON) and --input-file (a filesystem path, or "-" for stdin) flags. It
// returns the raw bytes and whether any input was supplied, erroring if both flags
// carry a value or if the file/stdin can't be read. The bytes are NOT schema-checked
// here — the caller validates them against workflow.input exactly as it does inline
// --input, so the two paths are byte-for-byte equivalent downstream (validation,
// blob Put, RunState seeding).
func resolveRunInput(inlineJSON, filePath string, stdin io.Reader) ([]byte, bool, error) {
	haveInline := inlineJSON != ""
	haveFile := filePath != ""
	switch {
	case haveInline && haveFile:
		return nil, false, errors.New("--input and --input-file are mutually exclusive; supply run input one way")
	case haveFile && filePath == "-":
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, false, fmt.Errorf("read run input from stdin: %w", err)
		}
		return b, true, nil
	case haveFile:
		b, err := os.ReadFile(filePath)
		if err != nil {
			return nil, false, fmt.Errorf("read run input file %q: %w", filePath, err)
		}
		return b, true, nil
	case haveInline:
		return []byte(inlineJSON), true, nil
	default:
		return nil, false, nil
	}
}

// stdin returns the reader for `--input-file -`: the injected Runner.Stdin when set
// (tests), else os.Stdin (production). Consulted only on the stdin path.
func (r *Runner) stdin() io.Reader {
	if r.Stdin != nil {
		return r.Stdin
	}
	return os.Stdin
}
