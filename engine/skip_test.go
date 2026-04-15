package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/valbaudo/awf/ir"
)

func TestSkipUnwindError(t *testing.T) {
	su := &SkipUnwind{TargetPath: "", Reason: "human rejected"}
	got := su.Error()
	want := `engine: skip unwind to "" (reason: human rejected)`
	if got != want {
		t.Errorf("SkipUnwind.Error():\n got: %q\nwant: %q", got, want)
	}
}

func TestSkipUnwindErrorWithTarget(t *testing.T) {
	su := &SkipUnwind{TargetPath: "loop[0].body.iter-2", Reason: "early exit"}
	got := su.Error()
	want := `engine: skip unwind to "loop[0].body.iter-2" (reason: early exit)`
	if got != want {
		t.Errorf("SkipUnwind.Error() with target:\n got: %q\nwant: %q", got, want)
	}
}

func TestRunSkipReturnsSkipUnwind(t *testing.T) {
	skip := &ir.Skip{Reason: "early exit"}
	oc, err := runSkip(skip)
	if oc != OutcomeOK {
		t.Errorf("runSkip outcome: got %q, want %q", oc, OutcomeOK)
	}
	var su *SkipUnwind
	if !errors.As(err, &su) {
		t.Fatalf("runSkip err: errors.As(*SkipUnwind) = false, got %v (%T)", err, err)
	}
	if su.Reason != "early exit" {
		t.Errorf("SkipUnwind.Reason: got %q, want %q", su.Reason, "early exit")
	}
	if su.TargetPath != "" {
		t.Errorf("SkipUnwind.TargetPath: got %q, want \"\" (runSkip doesn't populate target)", su.TargetPath)
	}
}

func TestSkipUnwindRecognizedThroughWrap(t *testing.T) {
	// A SkipUnwind wrapped by fmt.Errorf("%w", ...) is still recognized via errors.As.
	// Important: intermediate frames (interpNodes / interpNode) propagate the tuple
	// without wrapping, but if any handler ever DOES wrap with fmt.Errorf("...: %w"),
	// errors.As must still see through.
	su := &SkipUnwind{Reason: "x"}
	wrapped := fmt.Errorf("interpreter: %w", su)
	var unwrapped *SkipUnwind
	if !errors.As(wrapped, &unwrapped) {
		t.Errorf("errors.As through fmt.Errorf wrap: got false, want true")
	}
	if unwrapped.Reason != "x" {
		t.Errorf("unwrapped Reason: got %q, want %q", unwrapped.Reason, "x")
	}
}
