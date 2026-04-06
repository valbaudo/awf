package ir

import "testing"

// Tests for the AWF3003/AWF3004/AWF3005 compose pass — see validate_compose.go.

func TestComposeDigestPinningAWF3003(t *testing.T) {
	ld := &LoadedDefinition{
		Workflow: &Workflow{
			ID: "compose", Version: 1,
			Containers: map[string]Container{"lab": {Compose: "lab/compose.yml", Service: "vuln"}},
			Graph:      NodeList{},
		},
		WorkflowPath: "/tmp/wf.yaml",
		ComposeFiles: map[string][]byte{
			"lab/compose.yml": []byte("services:\n  vuln:\n    image: nginx:latest\n"),
		},
	}
	assertErrorAt(t, Validate(ld), "AWF3003", "containers.lab")
}

func TestComposeMissingImage(t *testing.T) {
	ld := &LoadedDefinition{
		Workflow: &Workflow{
			ID: "compose", Version: 1,
			Containers: map[string]Container{"lab": {Compose: "lab/compose.yml", Service: "vuln"}},
			Graph:      NodeList{},
		},
		WorkflowPath: "/tmp/wf.yaml",
		ComposeFiles: map[string][]byte{
			"lab/compose.yml": []byte("services:\n  vuln:\n    build: .\n"),
		},
	}
	assertErrorAt(t, Validate(ld), "AWF3003", "containers.lab")
}

func TestComposeParseFailureAWF3004(t *testing.T) {
	ld := &LoadedDefinition{
		Workflow: &Workflow{
			ID: "compose", Version: 1,
			Containers: map[string]Container{"lab": {Compose: "lab/compose.yml", Service: "vuln"}},
			Graph:      NodeList{},
		},
		WorkflowPath: "/tmp/wf.yaml",
		ComposeFiles: map[string][]byte{
			"lab/compose.yml": []byte("not valid yaml\n  - oops\n"),
		},
	}
	assertErrorAt(t, Validate(ld), "AWF3004", "containers.lab")
}

func TestComposeRejectsExtendsAWF3005(t *testing.T) {
	// extends directives can follow ABSOLUTE disk paths (verified empirically); we refuse
	// before handing bytes to compose-go.
	ld := &LoadedDefinition{
		Workflow: &Workflow{
			ID: "ext", Version: 1,
			Containers: map[string]Container{"lab": {Compose: "lab/compose.yml", Service: "vuln"}},
			Graph:      NodeList{},
		},
		WorkflowPath: "/tmp/wf.yaml",
		ComposeFiles: map[string][]byte{
			"lab/compose.yml": []byte("services:\n  vuln:\n    extends:\n      file: /etc/passwd\n      service: base\n"),
		},
	}
	assertErrorAt(t, Validate(ld), "AWF3005", "containers.lab")
}

func TestComposeRejectsIncludeAWF3005(t *testing.T) {
	ld := &LoadedDefinition{
		Workflow: &Workflow{
			ID: "inc", Version: 1,
			Containers: map[string]Container{"lab": {Compose: "lab/compose.yml", Service: "vuln"}},
			Graph:      NodeList{},
		},
		WorkflowPath: "/tmp/wf.yaml",
		ComposeFiles: map[string][]byte{
			"lab/compose.yml": []byte("include:\n  - path: /etc/passwd\nservices:\n  vuln:\n    image: example.com/x@sha256:0000000000000000000000000000000000000000000000000000000000000000\n"),
		},
	}
	assertErrorAt(t, Validate(ld), "AWF3005", "containers.lab")
}

func TestComposeAllDigestPinnedPasses(t *testing.T) {
	good := []byte("services:\n  vuln:\n    image: example.com/vuln@sha256:0000000000000000000000000000000000000000000000000000000000000000\n")
	ld := &LoadedDefinition{
		Workflow: &Workflow{
			ID: "compose", Version: 1,
			Containers: map[string]Container{"lab": {Compose: "lab/compose.yml", Service: "vuln"}},
			Graph:      NodeList{},
		},
		WorkflowPath: "/tmp/wf.yaml",
		ComposeFiles: map[string][]byte{"lab/compose.yml": good},
	}
	for _, d := range Validate(ld) {
		if d.Code == "AWF3003" || d.Code == "AWF3004" || d.Code == "AWF3005" {
			t.Errorf("did not expect AWF3003/AWF3004/AWF3005: %v", d)
		}
	}
}

func TestComposeEnvFileDoesNotReadDisk(t *testing.T) {
	// SECURITY: env_file references must not be followed to disk during validation.
	// Without SkipResolveEnvironment, compose-go would attempt to read the path; we verify
	// here by using a definitely-non-existent absolute path. WITH the fix, the compose
	// loads cleanly (env_file is simply not resolved) and the validator only checks
	// digest-pinning. WITHOUT the fix, compose-go would error "failed to read /does/not/exist".
	ld := &LoadedDefinition{
		Workflow: &Workflow{
			ID: "envfile", Version: 1,
			Containers: map[string]Container{"lab": {Compose: "lab/compose.yml", Service: "vuln"}},
			Graph:      NodeList{},
		},
		WorkflowPath: "/tmp/wf.yaml",
		ComposeFiles: map[string][]byte{
			"lab/compose.yml": []byte("services:\n  vuln:\n    image: example.com/x@sha256:0000000000000000000000000000000000000000000000000000000000000000\n    env_file:\n      - /does/not/exist/at/all.env\n"),
		},
	}
	// No AWF3003 (image is pinned), no AWF3004 (compose loads), no AWF3005 (no extends/include).
	for _, d := range Validate(ld) {
		if d.Code == "AWF3003" || d.Code == "AWF3004" || d.Code == "AWF3005" {
			t.Errorf("env_file should not cause a compose error: %v", d)
		}
	}
}

func TestComposeLabelFileBlockedAWF3005(t *testing.T) {
	// label_file: is a file-following directive that compose-go has no Skip option for —
	// the pre-scan is its only defense. A workflow author can't sneak `label_file: /etc/shadow`
	// past the validator.
	ld := &LoadedDefinition{
		Workflow: &Workflow{
			ID: "lblfile", Version: 1,
			Containers: map[string]Container{"lab": {Compose: "lab/compose.yml", Service: "vuln"}},
			Graph:      NodeList{},
		},
		WorkflowPath: "/tmp/wf.yaml",
		ComposeFiles: map[string][]byte{
			"lab/compose.yml": []byte("services:\n  vuln:\n    image: example.com/x@sha256:0000000000000000000000000000000000000000000000000000000000000000\n    label_file: /etc/shadow\n"),
		},
	}
	assertErrorAt(t, Validate(ld), "AWF3005", "containers.lab")
}
