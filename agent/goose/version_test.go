package goose_test

import (
	"context"
	"testing"

	"github.com/valbaudo/awf/agent/goose"
	"github.com/valbaudo/awf/container"
)

func TestVersion_LeadingSpace(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	// goose --version prints " 1.36.0\n" — ONE leading space, no program name.
	f.ProgramExecAny(container.ExecResult{ExitCode: 0, Stdout: []byte(" 1.36.0\n")}, nil)
	a, _ := goose.New(goose.WithBackend(f))
	v, err := a.Version(context.Background(), h)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != "1.36.0" {
		t.Errorf("Version = %q, want %q", v, "1.36.0")
	}
}

func TestVersion_NonzeroExit_RuntimeNotFound(t *testing.T) {
	f := container.NewFake()
	h, _ := f.Create(context.Background(), container.ContainerSpec{Name: "lab"})
	f.ProgramExecAny(container.ExecResult{ExitCode: 127}, nil)
	a, _ := goose.New(goose.WithBackend(f))
	if _, err := a.Version(context.Background(), h); err == nil {
		t.Fatal("Version: want error on nonzero exit")
	}
}

func TestVersion_NilBackend(t *testing.T) {
	a, _ := goose.New()
	if _, err := a.Version(context.Background(), container.Handle{Name: "lab"}); err == nil {
		t.Fatal("Version: want error when no backend wired")
	}
}
