package ir

import (
	"encoding/json"
	"fmt"
	"sort"
)

// OutputFile is one captured artifact. Name is the handle name for the NAMED
// (map) wire form; empty for the bare-list form (capture-only, unreferenceable).
// Never struct-marshaled (OutputFiles has a custom MarshalJSON) → no json tags,
// not in ir/tags_test.go's irTypes().
type OutputFile struct {
	Name string
	Path string
}

// OutputFiles unmarshals from EITHER a bare list (["/out/a"] → unnamed) OR a
// name→path map ({"report":"/out/r.md"} → named, referenceable as
// step.<id>.files.<name>). MarshalJSON re-emits the original shape so bare-list
// workflows stay byte-identical (digest-stable).
type OutputFiles []OutputFile

func (o *OutputFiles) UnmarshalJSON(b []byte) error {
	var list []string
	if err := json.Unmarshal(b, &list); err == nil {
		out := make(OutputFiles, 0, len(list))
		for _, p := range list {
			out = append(out, OutputFile{Path: p})
		}
		*o = out
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("output_files must be a list of paths or a name→path map: %w", err)
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make(OutputFiles, 0, len(m))
	for _, n := range names {
		out = append(out, OutputFile{Name: n, Path: m[n]})
	}
	*o = out
	return nil
}

func (o OutputFiles) MarshalJSON() ([]byte, error) {
	named := false
	for _, e := range o {
		if e.Name != "" {
			named = true
			break
		}
	}
	if !named {
		paths := make([]string, 0, len(o))
		for _, e := range o {
			paths = append(paths, e.Path)
		}
		return json.Marshal(paths)
	}
	m := make(map[string]string, len(o))
	for _, e := range o {
		m[e.Name] = e.Path
	}
	return json.Marshal(m)
}

// Paths returns the in-container paths to capture (Backend.CaptureFiles), in
// declaration order. nil for empty. Unchanged capture semantics.
func (o OutputFiles) Paths() []string {
	if len(o) == 0 {
		return nil
	}
	ps := make([]string, 0, len(o))
	for _, e := range o {
		ps = append(ps, e.Path)
	}
	return ps
}

// PathForName returns the declared container path for a named artifact (ok=false
// if no named entry matches). Used by the engine resolver to map
// step.<id>.files.<name> → the key in NodeResult.Files.
func (o OutputFiles) PathForName(name string) (string, bool) {
	for _, e := range o {
		if e.Name == name {
			return e.Path, true
		}
	}
	return "", false
}

// OutputFilesByStepID indexes every code/agent step's output_files by id in ONE
// graph walk. Shared by input_files validation (ir/validate_input_files.go) and
// engine resolution (engine.resolveInputFiles) so neither walks per-ref.
func OutputFilesByStepID(wf *Workflow) map[string]OutputFiles {
	out := map[string]OutputFiles{}
	WalkNodes(wf.Graph, "", func(n Node, _ string) {
		switch s := n.(type) {
		case *CodeStep:
			out[s.ID] = s.OutputFiles
		case *AgentStep:
			out[s.ID] = s.OutputFiles
		}
	})
	return out
}
