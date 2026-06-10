package ir

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// OutputFile is one captured artifact. Name is the handle name for the NAMED
// (map) wire form; empty for the bare-list form (capture-only, unreferenceable).
// Never struct-marshaled (OutputFiles has a custom MarshalJSON) → no json tags,
// not in ir/tags_test.go's irTypes().
type OutputFile struct {
	Name      string
	Path      string
	Format    string
	Schema    *JSONSchema
	SchemaRef string
}

type ArtifactContract struct {
	Format    string
	Schema    *JSONSchema
	SchemaRef string
}

type WorkflowInputFiles map[string]ArtifactContract

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
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("output_files must be a list of paths or a name→path/contract map: %w", err)
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make(OutputFiles, 0, len(m))
	for _, n := range names {
		var path string
		if err := json.Unmarshal(m[n], &path); err == nil {
			out = append(out, OutputFile{Name: n, Path: path})
			continue
		}
		var wire outputFileContractWire
		dec := json.NewDecoder(bytes.NewReader(m[n]))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&wire); err != nil {
			return fmt.Errorf("output_files[%q] must be a path string or contract object: %w", n, err)
		}
		contract := artifactContractFromOutputFileWire(wire)
		out = append(out, OutputFile{
			Name:      n,
			Path:      wire.Path,
			Format:    contract.Format,
			Schema:    contract.Schema,
			SchemaRef: contract.SchemaRef,
		})
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
	m := make(map[string]any, len(o))
	for _, e := range o {
		if e.Format == "" && e.Schema == nil && e.SchemaRef == "" {
			m[e.Name] = e.Path
			continue
		}
		contract := ContractFromOutputFile(e)
		m[e.Name] = outputFileContractWire{
			Path:      e.Path,
			Format:    contract.Format,
			Schema:    contract.Schema,
			SchemaRef: contract.SchemaRef,
		}
	}
	return json.Marshal(m)
}

func (w *WorkflowInputFiles) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("input_files must be a name→contract map: %w", err)
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make(WorkflowInputFiles, len(m))
	for _, n := range names {
		var wire artifactContractWire
		dec := json.NewDecoder(bytes.NewReader(m[n]))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&wire); err != nil {
			return fmt.Errorf("input_files[%q] must be a contract object: %w", n, err)
		}
		out[n] = artifactContractFromWire(wire)
	}
	*w = out
	return nil
}

func (w WorkflowInputFiles) MarshalJSON() ([]byte, error) {
	m := make(map[string]artifactContractWire, len(w))
	for name, contract := range w {
		m[name] = artifactContractWireFromContract(contract)
	}
	return json.Marshal(m)
}

func ContractFromOutputFile(of OutputFile) ArtifactContract {
	return ArtifactContract{
		Format:    of.Format,
		Schema:    of.Schema,
		SchemaRef: of.SchemaRef,
	}
}

type outputFileContractWire struct {
	Path      string      `json:"path"`
	Format    string      `json:"format,omitempty"`
	Schema    *JSONSchema `json:"schema,omitempty"`
	SchemaRef string      `json:"schema_ref,omitempty"`
}

type artifactContractWire struct {
	Format    string      `json:"format,omitempty"`
	Schema    *JSONSchema `json:"schema,omitempty"`
	SchemaRef string      `json:"schema_ref,omitempty"`
}

func artifactContractFromOutputFileWire(wire outputFileContractWire) ArtifactContract {
	return ArtifactContract{
		Format:    wire.Format,
		Schema:    wire.Schema,
		SchemaRef: wire.SchemaRef,
	}
}

func artifactContractFromWire(wire artifactContractWire) ArtifactContract {
	return ArtifactContract(wire)
}

func artifactContractWireFromContract(contract ArtifactContract) artifactContractWire {
	return artifactContractWire(contract)
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

// OutputFilesByStepID indexes every code/agent step's and reduced map product's
// output_files by id in ONE
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
		case *Map:
			if s.ID != "" && s.Reduce != nil {
				out[s.ID] = s.Reduce.OutputFiles
			}
		}
	})
	return out
}
