package droid_test

import (
	"context"
	"errors"
	"testing"

	"github.com/valbaudo/awf/agent/droid"
	"github.com/valbaudo/awf/container"
)

func TestVersion_ParsesBareSemver(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExec("droid --version", container.ExecResult{ExitCode: 0, Stdout: []byte("0.138.0\n")}, nil)
	a, _ := droid.New(droid.WithBackend(f))
	got, err := a.Version(context.Background(), h)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != "0.138.0" {
		t.Errorf("Version = %q, want 0.138.0", got)
	}
}

func TestVersion_NonzeroExit_NotFound(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExec("droid --version", container.ExecResult{ExitCode: 127}, nil)
	a, _ := droid.New(droid.WithBackend(f))
	_, err := a.Version(context.Background(), h)
	var nf *droid.ErrRuntimeNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v, want *droid.ErrRuntimeNotFound", err)
	}
}

func TestVersion_NilBackend(t *testing.T) {
	a, _ := droid.New()
	if _, err := a.Version(context.Background(), container.Handle{Name: "lab"}); err == nil {
		t.Fatal("Version with nil backend: err = nil, want error")
	}
}

func TestVersion_NoOutput_NotFound(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExec("droid --version", container.ExecResult{ExitCode: 0, Stdout: []byte("  \n")}, nil)
	a, _ := droid.New(droid.WithBackend(f))
	_, err := a.Version(context.Background(), h)
	var nf *droid.ErrRuntimeNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v, want *droid.ErrRuntimeNotFound for empty output", err)
	}
}

func TestVersion_NonSemverFallback(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExec("droid --version", container.ExecResult{ExitCode: 0, Stdout: []byte("droid version 1.2.3 (build abc)\n")}, nil)
	a, _ := droid.New(droid.WithBackend(f))
	got, err := a.Version(context.Background(), h)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	// The semver regex is anchored at start; this line does not begin with a digit,
	// so Version falls back to returning the whole first non-empty line.
	if got != "droid version 1.2.3 (build abc)" {
		t.Errorf("Version = %q, want the raw first line (non-semver fallback)", got)
	}
}
