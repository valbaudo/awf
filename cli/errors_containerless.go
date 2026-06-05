package cli

import "fmt"

// ErrContainerRequired is returned at run start (and resume) when an agent
// step omits `container:` but its resolved adapter is NOT Containerless (a
// CLI-wrapping adapter like claude/droid/goose needs a container to exec in).
// Permanent: re-running won't fix a missing container. This is where the
// missing-container check for non-containerless adapters lives, because the
// structural validator (ir/) is registry-free and cannot know an adapter's
// Containerless capability.
type ErrContainerRequired struct {
	Ref string
}

func (e *ErrContainerRequired) Error() string {
	return fmt.Sprintf("cli: agent runtime %q requires a container, but the step declares none (only Containerless adapters may omit `container:`)", e.Ref)
}
