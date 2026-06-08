//go:build integ

package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"

	"github.com/valbaudo/awf/cli"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

type runtimeComposeRunResult struct {
	rc     int
	stdout string
	stderr string
}

func TestCLIRunDockerRuntimeComposePromotion(t *testing.T) {
	dockerCli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("NewClientWithOpts: %v", err)
	}
	defer func() { _ = dockerCli.Close() }()
	pullImage(t, dockerCli, alpineDigest)

	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	runID := "runtime-compose-good"
	registerComposeProjectCleanup(t, dockerCli, runID)

	runnerCompose := filepath.Join(tmp, "runner-compose.yml")
	if err := os.WriteFile(runnerCompose, []byte(`
services:
  runner:
    image: `+alpineDigest+`
    command: ["sh", "-c", "sleep 86400"]
`), 0o600); err != nil {
		t.Fatal(err)
	}

	wfPath := filepath.Join(tmp, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`
workflow: runtime-compose-good
version: 1
containers:
  runner:
    compose: ./runner-compose.yml
    service: runner
graph:
  - id: lab_gen
    container: runner
    run: |
      mkdir -p /work/lab
      cat > /work/lab/compose.yml <<'EOF'
      services:
        web:
          image: `+alpineDigest+`
          command: ["sh", "-c", "printf web > /tmp/awf-svc-marker && sleep 86400"]
        api:
          image: `+alpineDigest+`
          command: ["sh", "-c", "printf api > /tmp/awf-svc-marker && sleep 86400"]
      EOF
      mkdir -p "$(dirname "$AWF_OUTPUT")"
      printf '{"service":"web"}' > "$AWF_OUTPUT"
    output_schema:
      type: object
      additionalProperties: false
      required: [service]
      properties:
        service: { type: string }
    output_files:
      compose: /work/lab/compose.yml
  - compose:
      as: lab
      from: step.lab_gen.files.compose
      service: "{{ step.lab_gen.service }}"
      body:
        - id: smoke_web
          container: lab
          run: cat /tmp/awf-svc-marker
        - id: smoke_api
          container: lab:api
          run: cat /tmp/awf-svc-marker
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	runner := &cli.Runner{}
	rc := runner.Run([]string{"run", "--backend", "docker", "--state-dir", stateDir, "--run-id", runID, wfPath}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK\nstderr:\n%s\nstdout:\n%s", rc, stderr.String(), stdout.String())
	}
	for _, want := range []string{"[smoke_web] web", "[smoke_api] api"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
	assertNoAWFComposeContainers(t, dockerCli, runID)
}

func TestCLIRunDockerRuntimeComposeInvalidCaughtBeforeCreate(t *testing.T) {
	dockerCli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("NewClientWithOpts: %v", err)
	}
	defer func() { _ = dockerCli.Close() }()
	pullImage(t, dockerCli, alpineDigest)

	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	runID := "runtime-compose-invalid"
	registerComposeProjectCleanup(t, dockerCli, runID)

	runnerCompose := filepath.Join(tmp, "runner-compose.yml")
	if err := os.WriteFile(runnerCompose, []byte(`
services:
  runner:
    image: `+alpineDigest+`
    command: ["sh", "-c", "sleep 86400"]
`), 0o600); err != nil {
		t.Fatal(err)
	}

	wfPath := filepath.Join(tmp, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`
workflow: runtime-compose-invalid
version: 1
containers:
  runner:
    compose: ./runner-compose.yml
    service: runner
graph:
  - id: lab_gen
    container: runner
    run: |
      mkdir -p /work/lab
      cat > /work/lab/compose.yml <<'EOF'
      not valid yaml
        - oops
      EOF
      mkdir -p "$(dirname "$AWF_OUTPUT")"
      printf '{"service":"web"}' > "$AWF_OUTPUT"
    output_schema:
      type: object
      additionalProperties: false
      required: [service]
      properties:
        service: { type: string }
    output_files:
      compose: /work/lab/compose.yml
  - try:
      do:
        - compose:
            as: lab
            from: step.lab_gen.files.compose
            service: "{{ step.lab_gen.service }}"
            body:
              - id: smoke
                container: lab
                run: "true"
      catch:
        - id: cannot_build_lab
          container: runner
          run: printf cannot_build_lab
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	runner := &cli.Runner{}
	rc := runner.Run([]string{"run", "--backend", "docker", "--state-dir", stateDir, "--run-id", runID, wfPath}, &stdout, &stderr)
	if rc != cli.ExitOK {
		t.Fatalf("rc = %d, want ExitOK via try.catch\nstderr:\n%s\nstdout:\n%s", rc, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "[cannot_build_lab] cannot_build_lab") {
		t.Fatalf("stdout missing catch sentinel:\n%s", stdout.String())
	}
	assertNoAWFComposeContainers(t, dockerCli, runID)
}

func TestCLIResumeDockerRuntimeComposeRepromotesAndSkipsCommittedBodyStep(t *testing.T) {
	dockerCli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("NewClientWithOpts: %v", err)
	}
	defer func() { _ = dockerCli.Close() }()
	pullImage(t, dockerCli, alpineDigest)

	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	runID := "runtime-compose-resume"
	registerComposeProjectCleanup(t, dockerCli, runID)

	runnerCompose := filepath.Join(tmp, "runner-compose.yml")
	if err := os.WriteFile(runnerCompose, []byte(`
services:
  runner:
    image: `+alpineDigest+`
    command: ["sh", "-c", "sleep 86400"]
`), 0o600); err != nil {
		t.Fatal(err)
	}

	wfPath := filepath.Join(tmp, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(`
workflow: runtime-compose-resume
version: 1
containers:
  runner:
    compose: ./runner-compose.yml
    service: runner
graph:
  - id: lab_gen
    container: runner
    run: |
      mkdir -p /work/lab
      cat > /work/lab/compose.yml <<'EOF'
      services:
        web:
          image: `+alpineDigest+`
          command: ["sh", "-c", "sleep 86400"]
      EOF
      mkdir -p "$(dirname "$AWF_OUTPUT")"
      printf '{"service":"web"}' > "$AWF_OUTPUT"
    output_schema:
      type: object
      additionalProperties: false
      required: [service]
      properties:
        service: { type: string }
    output_files:
      compose: /work/lab/compose.yml
  - compose:
      as: lab
      from: step.lab_gen.files.compose
      service: "{{ step.lab_gen.service }}"
      body:
        - id: first_marker
          container: lab
          run: printf first
        - id: wait_for_resume
          await: runtime_resume_marker
        - id: second_marker
          container: lab
          run: printf second
`), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan runtimeComposeRunResult, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		runner := &cli.Runner{}
		rc := runner.Run([]string{"run", "--backend", "docker", "--state-dir", stateDir, "--run-id", runID, wfPath}, &stdout, &stderr)
		done <- runtimeComposeRunResult{rc: rc, stdout: stdout.String(), stderr: stderr.String()}
	}()

	waitForCommittedStep(t, stateDir, runID, "compose[1].body.first_marker")

	var pstdout, pstderr bytes.Buffer
	pauseRunner := &cli.Runner{}
	prc := pauseRunner.Run([]string{"pause", "--state-dir", stateDir, runID, "--reason", "runtime compose resume test"}, &pstdout, &pstderr)
	if prc != cli.ExitOK {
		t.Fatalf("pause rc = %d; stderr: %s", prc, pstderr.String())
	}
	select {
	case result := <-done:
		if result.rc != cli.ExitOK {
			t.Fatalf("first run rc = %d, want ExitOK after pause\nstderr:\n%s\nstdout:\n%s", result.rc, result.stderr, result.stdout)
		}
	case <-time.After(runReturnAfterPauseLimit):
		t.Fatal("first run did not return within timeout after pause")
	}

	var sigStdout, sigStderr bytes.Buffer
	sigRunner := &cli.Runner{}
	src := sigRunner.Run([]string{"signal", "--state-dir", stateDir, runID, "runtime_resume_marker"}, &sigStdout, &sigStderr)
	if src != cli.ExitOK {
		t.Fatalf("signal rc = %d; stderr: %s", src, sigStderr.String())
	}

	var resumeStdout, resumeStderr bytes.Buffer
	resumeRunner := &cli.Runner{}
	rc := resumeRunner.Run([]string{"resume", "--state-dir", stateDir, runID, wfPath}, &resumeStdout, &resumeStderr)
	if rc != cli.ExitOK {
		t.Fatalf("resume rc = %d, want ExitOK\nstderr:\n%s\nstdout:\n%s", rc, resumeStderr.String(), resumeStdout.String())
	}
	if strings.Contains(resumeStdout.String(), "[first_marker] first") {
		t.Fatalf("resume re-ran committed first_marker step:\n%s", resumeStdout.String())
	}
	if !strings.Contains(resumeStdout.String(), "[second_marker] second") {
		t.Fatalf("resume stdout missing second_marker, so runtime compose was not re-promoted for the uncommitted body frontier:\n%s", resumeStdout.String())
	}

	events, err := state.FoldFile(filepath.Join(stateDir, "runs", runID, "log"))
	if err != nil {
		t.Fatalf("FoldFile: %v", err)
	}
	completed := map[string]int{}
	for _, e := range events {
		if e.Type == engine.EventNodeCompleted {
			completed[e.Path]++
		}
	}
	if completed["compose[1].body.first_marker"] != 1 {
		t.Fatalf("first_marker completed %d time(s), want 1", completed["compose[1].body.first_marker"])
	}
	if completed["compose[1].body.wait_for_resume"] != 1 {
		t.Fatalf("wait_for_resume completed %d time(s), want 1", completed["compose[1].body.wait_for_resume"])
	}
	if completed["compose[1].body.second_marker"] != 1 {
		t.Fatalf("second_marker completed %d time(s), want 1", completed["compose[1].body.second_marker"])
	}
	assertNoAWFComposeContainers(t, dockerCli, runID)
}

func assertNoAWFComposeContainers(t *testing.T, cli *dockerclient.Client, runID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	containers, err := cli.ContainerList(ctx, dockerContainer.ListOptions{All: true})
	if err != nil {
		t.Fatalf("ContainerList: %v", err)
	}
	prefix := "awf-" + runID + "-"
	for _, c := range containers {
		project := c.Labels["com.docker.compose.project"]
		if strings.HasPrefix(project, prefix) {
			t.Fatalf("found leftover compose container %s project=%s names=%v", c.ID, project, c.Names)
		}
	}
}
