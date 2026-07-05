package loader

import "fmt"

type LoadError struct {
	Code    string
	Source  string
	Path    string
	Message string
	Err     error
}

// Detail composes the human message with the wrapped cause (goccy's
// caret-annotated location survives). "Message: Err" per the Go %w convention.
func (e *LoadError) Detail() string {
	if e.Message != "" && e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return ""
}

func (e *LoadError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := e.Detail()
	if e.Source != "" && e.Path != "" {
		return fmt.Sprintf("%s: %s at %s %s", e.Code, msg, e.Source, e.Path)
	}
	if e.Source != "" {
		return fmt.Sprintf("%s: %s at %s", e.Code, msg, e.Source)
	}
	if e.Path != "" {
		return fmt.Sprintf("%s: %s at %s", e.Code, msg, e.Path)
	}
	return fmt.Sprintf("%s: %s", e.Code, msg)
}

func (e *LoadError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
