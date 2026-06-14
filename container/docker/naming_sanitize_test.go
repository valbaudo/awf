package docker

import (
	"regexp"
	"testing"

	composeloader "github.com/compose-spec/compose-go/v2/loader"
)

// dockerContainerNamePattern mirrors Docker's daemon-side container-name rule
// (moby/moby daemon/names: ^[a-zA-Z0-9][a-zA-Z0-9_.-]+$). ContainerCreate is
// rejected server-side when the requested name violates it — notably ":" is
// NOT permitted (".", "_", "-" are). The trailing "+" (min length 2) matches
// Docker exactly; our names always start with the "awf-" prefix so they clear
// it, but the pattern stays faithful to the daemon rule.
var dockerContainerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// Subworkflow-declared containers are addressed by a QualifiedContainerKey such
// as "prepare_lab.workflow::labgen" (engine/path.go) — it carries "::" and the
// ".workflow" boundary segment. That key flows into spec.Name and then into
// containerName(); the "::" makes the resulting Docker container name invalid,
// so image-mode Create is rejected by the daemon. containerName must yield a
// name Docker accepts.
func TestContainerNameSanitizesRuntimeKey(t *testing.T) {
	got := containerName("0a1b2c3d", "prepare_lab.workflow::labgen")
	if !dockerContainerNamePattern.MatchString(got) {
		t.Errorf("containerName produced a Docker-invalid container name %q (must match %s)",
			got, dockerContainerNamePattern)
	}
}

// compose-go's loader (SetProjectName(name, true) in compose.go) rejects a
// project name unless NormalizeProjectName(name) == name (lowercase, only
// [a-z0-9_-]). The same subworkflow key flows into composeProjectName(); the
// "." and "::" make compose Up() fail with "invalid project name". The minted
// project name must already satisfy compose-go's own normalizer.
func TestComposeProjectNameSanitizesRuntimeKey(t *testing.T) {
	got := composeProjectName("0a1b2c3d", "prepare_lab.workflow::labgen")
	if norm := composeloader.NormalizeProjectName(got); norm != got {
		t.Errorf("composeProjectName produced a compose-invalid project name %q; compose-go would reject it (normalized form is %q)",
			got, norm)
	}
}

// An UPPERCASE declared segment (container map keys are not charset-validated by
// the IR, and runtime-parent paths derive from step ids that permit uppercase)
// must still yield a valid compose project name, since compose-go requires
// lowercase.
func TestComposeProjectNameLowercasesUppercaseSegment(t *testing.T) {
	got := composeProjectName("0a1b2c3d", "PrepareLab.workflow::LabGen")
	if norm := composeloader.NormalizeProjectName(got); norm != got {
		t.Errorf("composeProjectName(uppercase key) = %q is not compose-valid (normalized %q)", got, norm)
	}
}

// Forbidden characters must be REPLACED with "-", not deleted: deleting them
// (as compose-go's NormalizeProjectName does on its own) collapses keys that
// differ only by separators onto the same project name, silently clobbering
// the per-container handle/project. Two sibling compose containers in one run
// keyed "lab.db" and "labdb" must therefore mint distinct project names.
func TestComposeProjectNameKeepsSeparatorKeysDistinct(t *testing.T) {
	dotted := composeProjectName("0a1b2c3d", "lab.db")
	plain := composeProjectName("0a1b2c3d", "labdb")
	if dotted == plain {
		t.Errorf("composeProjectName collapsed distinct keys onto %q (lab.db and labdb must stay distinct)", dotted)
	}
}

// Same invariant for image-mode container names: "lab.db" keeps its "." while
// "labdb" has none, so the two never collide.
func TestContainerNameKeepsSeparatorKeysDistinct(t *testing.T) {
	dotted := containerName("0a1b2c3d", "lab:db")
	plain := containerName("0a1b2c3d", "labdb")
	if dotted == plain {
		t.Errorf("containerName collapsed distinct keys onto %q (lab:db and labdb must stay distinct)", dotted)
	}
}
