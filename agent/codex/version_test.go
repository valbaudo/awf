package codex_test

import (
	"context"
	"testing"

	"github.com/valbaudo/awf/agent/codex"
	"github.com/valbaudo/awf/container"
)

func TestVersion_PrefixedSemver(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	// codex --version prints "codex-cli 0.131.0\n" — a program-name PREFIX, so the
	// semver regex must be UN-anchored (goose's ^-anchored pattern would miss).
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: []byte("codex-cli 0.131.0\n")}, nil)
	a, _ := codex.New(codex.WithBackend(f))
	v, err := a.Version(context.Background(), h)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != "0.131.0" {
		t.Errorf("Version = %q, want %q", v, "0.131.0")
	}
}

func TestVersion_NonzeroExit_RuntimeNotFound(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 127}, nil)
	a, _ := codex.New(codex.WithBackend(f))
	if _, err := a.Version(context.Background(), h); err == nil {
		t.Fatal("Version: want error on nonzero exit")
	}
}

func TestVersion_NilBackend(t *testing.T) {
	a, _ := codex.New()
	if _, err := a.Version(context.Background(), container.Handle{Name: "lab"}); err == nil {
		t.Fatal("Version: want error when no backend wired")
	}
}
