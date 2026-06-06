package cli

import "fmt"

// ErrThreadedRequired is returned at run start (and resume) when an agent step
// declares `continues:` (engine-owned conversation threading) but its resolved
// adapter does NOT report Caps.Threaded — i.e. the adapter cannot prepend an
// engine-supplied AgentInvocation.Thread, so threading would silently no-op.
// Permanent: re-running won't make a non-threaded adapter understand a thread.
// This lives in cli/ (not ir/) because the structural validator is registry-
// free and cannot know an adapter's Threaded capability — the same constraint
// that puts the Containerless guard (ErrContainerRequired) here.
type ErrThreadedRequired struct {
	StepID string // the step that declares continues:
	Ref    string // its uses: ref (the non-threaded adapter)
}

func (e *ErrThreadedRequired) Error() string {
	return fmt.Sprintf("cli: step %q declares `continues:` but its agent runtime %q does not support engine-threaded conversations (Caps.Threaded is false)", e.StepID, e.Ref)
}
