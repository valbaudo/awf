package docker

import (
	"context"
	"testing"
)

func TestComposeProjectName(t *testing.T) {
	got := composeProjectName("run-abc")
	want := "awf-run-abc"
	if got != want {
		t.Errorf("composeProjectName(\"run-abc\") = %q, want %q", got, want)
	}
}

func TestLoadComposeProjectOK(t *testing.T) {
	bytes := []byte(`services:
  web:
    image: nginx@sha256:0000000000000000000000000000000000000000000000000000000000000000
`)
	project, err := loadComposeProject(context.Background(), bytes, "lab/compose.yml", "awf-test")
	if err != nil {
		t.Fatalf("loadComposeProject: %v", err)
	}
	if project == nil {
		t.Fatal("project = nil")
	}
	if project.Name != "awf-test" {
		t.Errorf("project.Name = %q, want \"awf-test\"", project.Name)
	}
	if _, ok := project.Services["web"]; !ok {
		t.Errorf("services[\"web\"] missing; got services=%v", project.Services)
	}
}

func TestLoadComposeProjectMalformed(t *testing.T) {
	bytes := []byte("not-valid: yaml: bad: indentation\n")
	_, err := loadComposeProject(context.Background(), bytes, "lab/compose.yml", "awf-test")
	if err == nil {
		t.Fatal("loadComposeProject(malformed): err = nil, want non-nil")
	}
}
