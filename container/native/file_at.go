package native

import (
	"context"

	"github.com/valbaudo/awf/container"
)

// ReadFileAt / WriteFileAt are not yet implemented on the native backend
// (Milestone 2). Stubs keep the Backend interface satisfied so the build stays
// green; the fake is the only full impl this milestone.
func (b *Backend) ReadFileAt(ctx context.Context, h container.Handle, path string) ([]byte, error) {
	return nil, container.ErrUnsupported
}

func (b *Backend) WriteFileAt(ctx context.Context, h container.Handle, path string, content []byte) error {
	return container.ErrUnsupported
}
