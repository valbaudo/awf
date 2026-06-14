// Package docker implements container.Backend against the Docker Engine SDK.
// Phase 4 slices ship the implementation:
//   - 4.1: image-mode Create + Destroy + Capabilities skeleton.
//   - 4.2: real Exec + CaptureFiles (stdcopy demux + ctx-cancel watcher).
//   - 4.3: compose-mode Create + Exec + Destroy (compose-go + compose/v2).
//   - 4.4: Snapshot + Restore (streaming gzip-tar via ContainerDiff /
//     CopyFromContainer through state.Blobs; 3-segment SnapshotRef
//     embeds image + Cmd + Entrypoint; Restore streams via io.Pipe).
//
// See docs/superpowers/specs/2026-04-14-awf-phase4-design.md for the design.
package docker

import (
	"regexp"
	"strings"
)

// awfPrefix scopes Docker container names to AWF runs. Used by the orphan
// sweep to filter our containers from others on the same daemon (per Phase 4
// design decision 9).
const awfPrefix = "awf-"

// dockerNameUnsafe matches any run of characters Docker forbids in a container
// name. Docker's daemon-side rule is ^[a-zA-Z0-9][a-zA-Z0-9_.-]+$ — "." and "_"
// are allowed, but ":" (and anything else) is rejected at ContainerCreate. A
// declared name reaches us as a QualifiedContainerKey for subworkflow-scoped
// containers (engine/path.go), e.g. "prepare_lab.workflow::labgen", whose "::"
// would make the container name invalid; and the IR doesn't charset-validate
// container map keys at all. We replace each forbidden run with a single "-".
var dockerNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// composeNameUnsafe is the compose-project-name counterpart of dockerNameUnsafe.
// compose-go is stricter than Docker: a project name must satisfy
// NormalizeProjectName(name) == name, i.e. be lowercase and contain only
// [a-z0-9_-] ("." is NOT allowed, uppercase is NOT allowed). We lower-case the
// assembled name and replace each forbidden run with a single "-" — the SAME
// replace-with-dash strategy as dockerNameUnsafe (not compose-go's own
// NormalizeProjectName, which DELETES forbidden chars and so would fold
// "lab.db" and "labdb" onto one project name).
var composeNameUnsafe = regexp.MustCompile(`[^a-z0-9_-]+`)

// containerName formats the Docker container name per Phase 4 design
// decision 9: "awf-<run.id>-<declared>".
//
// The "awf-" prefix lets the orphan sweep filter our containers from others
// on the same daemon. The run.id segment scopes the name to one run, so
// parallel runs on one host don't collide.
//
// Format-only — there is no reverse parser. The IR doesn't currently enforce
// a charset on container map keys (the YAML loader accepts any map key
// string), so a declared name containing "-" makes a LastIndex-style split
// ambiguous. cleanupOrphans (backend_integ_test.go) uses strings.HasPrefix
// against containerPrefix(runID), which doesn't need parsing.
//
// Only the declared segment is sanitized; the "awf-<run.id>-" prefix is left
// verbatim so it stays an exact prefix of the result (the run.id is lowercase
// hex, already name-safe) and the orphan sweep's containerPrefix(runID) match
// keeps working. Sanitization is lossy and non-injective (a forbidden run and a
// literal "-" both fold to "-", so "a::b" and "a-b" both yield "a-b"); residual
// collisions are tolerable because every name is already run-id-scoped — the
// same property the engine's runtimeComposeName relies on — and the only keys
// that can still collide differ solely in already-invalid punctuation.
func containerName(runID, declared string) string {
	return awfPrefix + runID + "-" + dockerNameUnsafe.ReplaceAllString(declared, "-")
}

// containerPrefix returns the prefix used to filter containers belonging to
// a specific run. Slice 4.1's test cleanup uses this.
func containerPrefix(runID string) string {
	return awfPrefix + runID + "-"
}

// composeProjectName formats the Docker compose project name per Phase 4
// design decision 9: "awf-<run.id>-<declared>" — same scoping as image-mode
// container names. Including the declared container name lets a workflow
// declare multiple compose containers (AWF spec §3 permits this) without
// project-name collisions on the daemon.
//
// compose-go's loader (createCompose calls SetProjectName(name, true)) rejects
// any project name where NormalizeProjectName(name) != name — it must be
// lowercase and contain only [a-z0-9_-]. A subworkflow-scoped declared name
// such as "prepare_lab.workflow::labgen" carries "." and "::" and so is
// rejected. We lower-case the assembled name and collapse each forbidden run to
// a single "-": the result is, by construction, already a NormalizeProjectName
// fixed point (it is lowercase, only [a-z0-9_-], and the "awf-" prefix gives it
// a valid leading char), so SetProjectName accepts it — and replacing rather
// than deleting keeps "lab.db" ("lab-db") distinct from "labdb", which compose-
// go's own normalizer would not. The naming_sanitize_test asserts the output
// satisfies compose-go's NormalizeProjectName, guarding against rule drift.
// (Lower-casing the whole string is fine: compose has no prefix-match
// dependency like the image-mode orphan sweep, and run ids are lowercase hex.)
func composeProjectName(runID, declared string) string {
	return composeNameUnsafe.ReplaceAllString(strings.ToLower(awfPrefix+runID+"-"+declared), "-")
}
