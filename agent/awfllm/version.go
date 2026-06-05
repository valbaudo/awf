package awfllm

import (
	"context"

	"github.com/valbaudo/awf/container"
)

// adapterVersion is the pinned runtime identity. There is no binary to probe —
// the resolved MODEL is pinned via the workflow definition digest (with.model),
// so Version stays network-free at run-start and resume. Bump ONLY on an
// intentional adapter-contract revision (resume hard-errors on drift).
const adapterVersion = "awf-llm/1"

// Version returns the static adapter version. Ignores ctx and handle.
func (*Adapter) Version(_ context.Context, _ container.Handle) (string, error) {
	return adapterVersion, nil
}
