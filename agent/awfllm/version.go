package awfllm

import (
	"context"

	"github.com/valbaudo/awf/container"
)

// adapterVersion is the pinned runtime identity. There is no binary to probe, so
// Version stays network-free at run-start and resume. The MODEL is NOT part of
// this version pin: it may be fixed in the workflow definition (a literal
// role/step model:) OR selected at run time from input (a role's
// model: "{{ input.* }}"), in which case the resolved model rides the pinned
// run/call input (InputRef), replayed on resume — deterministic on resume, but
// not a definition-digest pin. Do not assume model is definition-fixed. Bump
// adapterVersion ONLY on an intentional adapter-contract revision (resume
// hard-errors on drift).
const adapterVersion = "awf-llm/1"

// Version returns the static adapter version. Ignores ctx and handle.
func (*Adapter) Version(_ context.Context, _ container.Handle) (string, error) {
	return adapterVersion, nil
}
