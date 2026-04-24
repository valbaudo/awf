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

// awfPrefix scopes Docker container names to AWF runs. Used by the orphan
// sweep to filter our containers from others on the same daemon (per Phase 4
// design decision 9).
const awfPrefix = "awf-"

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
func containerName(runID, declared string) string {
	return awfPrefix + runID + "-" + declared
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
func composeProjectName(runID, declared string) string {
	return awfPrefix + runID + "-" + declared
}
