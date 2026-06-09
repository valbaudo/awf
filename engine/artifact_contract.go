package engine

import (
	"bufio"
	"bytes"
	"fmt"
	"io"

	"github.com/valbaudo/awf/container"
)

func ValidateArtifactContract(path string, content []byte, contract OutputFileContract) error {
	if contract.Format == "" && contract.Schema == nil {
		return nil
	}
	switch contract.Format {
	case "json":
		if _, err := ValidateJSONAgainstSchema(content, contract.Schema); err != nil {
			return fmt.Errorf("artifact contract %s: json: %w", path, err)
		}
	case "jsonl":
		if err := validateJSONLArtifact(path, content, contract); err != nil {
			return err
		}
	default:
		return fmt.Errorf("artifact contract %s: unsupported format %q", path, contract.Format)
	}
	return nil
}

func validateCapturedArtifacts(files []container.CapturedFile, contracts map[string]OutputFileContract) error {
	if len(contracts) == 0 {
		return nil
	}
	for _, f := range files {
		contract, ok := contracts[f.Path]
		if !ok {
			continue
		}
		if err := ValidateArtifactContract(f.Path, f.Content, contract); err != nil {
			return err
		}
	}
	return nil
}

func validateJSONLArtifact(path string, content []byte, contract OutputFileContract) error {
	reader := bufio.NewReader(bytes.NewReader(content))
	lineNo := 0
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			line = bytes.TrimSuffix(line, []byte{'\n'})
			if len(bytes.TrimSpace(line)) == 0 {
				return fmt.Errorf("artifact contract %s: jsonl line %d: blank lines are invalid", path, lineNo)
			}
			if _, verr := ValidateJSONAgainstSchema(line, contract.Schema); verr != nil {
				return fmt.Errorf("artifact contract %s: jsonl line %d: %w", path, lineNo, verr)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("artifact contract %s: read jsonl: %w", path, err)
		}
	}
	if lineNo == 0 {
		return fmt.Errorf("artifact contract %s: jsonl: empty files are invalid", path)
	}
	return nil
}
