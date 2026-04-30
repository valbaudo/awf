package claude_test

import (
	"context"
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent/claude"
	"github.com/valbaudo/awf/container"
)

func TestVersion_ParsesSemverFromClaudeVersionOutput(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExec("claude --version", container.ExecResult{ExitCode: 0, Stdout: []byte("2.1.152 (Claude Code)\n")}, nil)
	a, _ := claude.New(claude.WithBackend(f))
	ver, err := a.Version(context.Background(), h)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if ver != "2.1.152" {
		t.Errorf("Version = %q, want %q", ver, "2.1.152")
	}
}

func TestVersion_ParsesSemverWithBuildHash(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExec("claude --version", container.ExecResult{ExitCode: 0, Stdout: []byte("2.1.152-abc123 (Claude Code)\n")}, nil)
	a, _ := claude.New(claude.WithBackend(f))
	ver, err := a.Version(context.Background(), h)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if ver != "2.1.152-abc123" {
		t.Errorf("Version = %q, want %q", ver, "2.1.152-abc123")
	}
}

func TestVersion_NoSemverFallsBackToFirstLine(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExec("claude --version", container.ExecResult{ExitCode: 0, Stdout: []byte("claude-code beta\n")}, nil)
	a, _ := claude.New(claude.WithBackend(f))
	ver, err := a.Version(context.Background(), h)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if ver != "claude-code beta" {
		t.Errorf("Version = %q; want first-line literal", ver)
	}
}

func TestVersion_NonzeroExit_ReturnsErrAgentRuntimeNotFound(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExec("claude --version", container.ExecResult{ExitCode: 127, Stdout: []byte("")}, nil)
	a, _ := claude.New(claude.WithBackend(f))
	_, err := a.Version(context.Background(), h)
	var nf *claude.ErrAgentRuntimeNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v; want *ErrAgentRuntimeNotFound", err)
	}
	if nf.Container != "lab" {
		t.Errorf("Container = %q", nf.Container)
	}
}

func TestVersion_NoBackend_Errors(t *testing.T) {
	a, _ := claude.New() // no WithBackend
	_, err := a.Version(context.Background(), container.Handle{Name: "lab", ID: "x"})
	if err == nil {
		t.Fatal("err nil; want non-nil when no Backend wired")
	}
}
