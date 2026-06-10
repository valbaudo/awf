package loader

import "fmt"

type LoadError struct {
	Code    string
	Source  string
	Path    string
	Message string
	Err     error
}

func (e *LoadError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := e.Message
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
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
