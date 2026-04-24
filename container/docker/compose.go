package docker

import (
	"context"
	"fmt"
	"io"
	"sort"

	composeloader "github.com/compose-spec/compose-go/v2/loader"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	cliflags "github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	cont "github.com/valbaudo/awf/container"
)

// createCompose handles the compose-mode branch of Backend.Create. Re-parses
// the compose bytes via compose-spec/compose-go/v2 (Design Q1 — same library
// the validator uses, no parse divergence), constructs an api.Compose
// service via docker/compose/v2 (Design Q5), and calls Up(... Wait: true)
// to bring every service to healthcheck-gated readiness (Design Q7 + spec §3).
//
// Returns Handle{Name: spec.Name, ID: <project name>, Service: spec.Service}.
// The Service field carries the default service the next Exec routes to;
// the dispatcher may override it for `container: lab:db` cross-service exec
// (Design Q4).
func (b *Backend) createCompose(ctx context.Context, spec cont.ContainerSpec) (cont.Handle, error) {
	if len(spec.Compose) == 0 {
		return cont.Handle{}, fmt.Errorf("container/docker: createCompose: spec.Compose is empty (loader/validator should have populated it)")
	}
	// Defensive: the validator (ir/validate_structural.go:44) already emits
	// AWF1008 for compose-without-service at validate time. This runtime check
	// is belt-and-suspenders against a constructor-bypass path (e.g. a test
	// that builds a ContainerSpec directly without going through loader+validator).
	if spec.Service == "" {
		return cont.Handle{}, fmt.Errorf("container/docker: createCompose: spec.Service is required (IR §3 `service:` field; validator AWF1008)")
	}

	projectName := composeProjectName(b.runID, spec.Name)
	project, err := loadComposeProject(ctx, spec.Compose, spec.ComposePath, projectName)
	if err != nil {
		return cont.Handle{}, fmt.Errorf("container/docker: createCompose: loadComposeProject: %w", err)
	}

	// Lazy-construct command.Cli + api.Compose on first compose-mode Create.
	// ensureComposeCli is race-safe via sync.Once.
	cli, err := b.ensureComposeCli()
	if err != nil {
		return cont.Handle{}, fmt.Errorf("container/docker: createCompose: ensureComposeCli: %w", err)
	}
	composeAPI := compose.NewComposeService(cli)

	// Up's positional `project` argument is authoritative — verified in
	// docker/compose v2.40.3 pkg/compose/up.go: the implementation passes
	// `project` to s.create and uses `project.Name` for Start, never reading
	// options.Start.Project. Only Wait is load-bearing for our use case
	// (healthcheck-gated readiness per spec §3 + Design Q7).
	if err := composeAPI.Up(ctx, project, api.UpOptions{
		Start: api.StartOptions{Wait: true},
	}); err != nil {
		return cont.Handle{}, fmt.Errorf("container/docker: createCompose: Up: %w", err)
	}

	b.mu.Lock()
	b.handles[projectName] = registeredContainer{
		kind:       kindCompose,
		project:    projectName,
		defaultSvc: spec.Service,
		composeAPI: composeAPI,
	}
	b.mu.Unlock()

	return cont.Handle{Name: spec.Name, ID: projectName, Service: spec.Service}, nil
}

// destroyCompose handles the compose-mode branch of Backend.Destroy. Calls
// api.Compose.Down with Volumes: true + RemoveOrphans: true.
//
// Errors are wrapped; on failure the registeredContainer is NOT re-recorded
// (compose state may be partially-down — caller should NOT retry; cleanupOrphans
// is the safety net).
func (b *Backend) destroyCompose(ctx context.Context, r registeredContainer) error {
	if r.composeAPI == nil {
		return fmt.Errorf("container/docker: destroyCompose: composeAPI is nil (internal state corrupt)")
	}
	if err := r.composeAPI.Down(ctx, r.project, api.DownOptions{
		RemoveOrphans: true,
		Volumes:       true,
	}); err != nil {
		return fmt.Errorf("container/docker: destroyCompose: Down: %w", err)
	}
	return nil
}

// execCompose handles the compose-mode branch of Backend.Exec. Resolves the
// target container via ContainerList filtered by project + service labels,
// then delegates to the slice-4.2 execImage path.
//
// Service resolution: h.Service if non-empty (cross-service exec from a
// dispatcher Handle clone, Design Q4), else r.defaultSvc.
func (b *Backend) execCompose(ctx context.Context, h cont.Handle, r registeredContainer, cmd cont.Cmd) (cont.ExecResult, <-chan cont.IOChunk, error) {
	svc := h.Service
	if svc == "" {
		svc = r.defaultSvc
	}
	dockerID, err := b.resolveComposeContainer(ctx, r.project, svc)
	if err != nil {
		return cont.ExecResult{}, nil, fmt.Errorf("container/docker: execCompose: %w", err)
	}
	return b.execImage(ctx, dockerID, cmd)
}

// resolveContainerID returns the Docker container ID a Backend method should
// target for a given handle. Image-mode: r.dockerID. Compose-mode: discovered
// via ContainerList(filter: label com.docker.compose.project + service).
//
// Used by CaptureFiles to resolve the container for tar-extract.
func (b *Backend) resolveContainerID(ctx context.Context, h cont.Handle, r registeredContainer) (string, error) {
	switch r.kind {
	case kindImage:
		return r.dockerID, nil
	case kindCompose:
		svc := h.Service
		if svc == "" {
			svc = r.defaultSvc
		}
		return b.resolveComposeContainer(ctx, r.project, svc)
	default:
		return "", fmt.Errorf("container/docker: resolveContainerID: unknown kind %q", r.kind)
	}
}

// resolveComposeContainer finds the Docker container ID for a (project,
// service) pair by querying ContainerList with the compose labels. Returns
// an error if 0 or >1 containers match.
func (b *Backend) resolveComposeContainer(ctx context.Context, project, service string) (string, error) {
	args := filters.NewArgs(
		filters.Arg("label", api.ProjectLabel+"="+project),
		filters.Arg("label", api.ServiceLabel+"="+service),
	)
	list, err := b.cli.ContainerList(ctx, dockerContainer.ListOptions{
		All:     true,
		Filters: args,
	})
	if err != nil {
		return "", fmt.Errorf("ContainerList: %w", err)
	}
	if len(list) == 0 {
		return "", fmt.Errorf("no container found for project=%q service=%q (compose Up may have failed silently)", project, service)
	}
	if len(list) > 1 {
		// Sort by ID for a deterministic error message.
		ids := make([]string, 0, len(list))
		for _, c := range list {
			ids = append(ids, c.ID)
		}
		sort.Strings(ids)
		return "", fmt.Errorf("multiple containers found for project=%q service=%q: %v (expected exactly 1)", project, service, ids)
	}
	return list[0].ID, nil
}

// loadComposeProject parses the compose YAML bytes into a *composetypes.Project
// using compose-spec/compose-go/v2 — the SAME library the validator uses.
//
// SECURITY-CRITICAL: SkipExtends, SkipInclude, and SkipResolveEnvironment are
// LOAD-BEARING. Without them, compose-go will read transitively-referenced
// files from disk during parse — including absolute paths like `extends: file:
// /etc/passwd` or `env_file: /etc/shadow`. Empirically verified against
// compose-go v2.11.0 (see ir/validate_compose.go's security comment).
// Removing ANY of these three options widens the file-read primitive at
// runtime. The validator's pre-scan layer (hasFileFollowingDirective in
// ir/validate_compose.go) is layer 1 protection; these Skip options are
// layer 2. A behavioral test cannot verify the file isn't opened without
// filesystem instrumentation — protection is enforced by CODE REVIEW of
// THIS COMMENT, not by a unit test.
//
// projectName is set explicitly via SetProjectName so the resulting Project's
// labels (com.docker.compose.project) match what resolveComposeContainer
// queries with.
//
// ctx is accepted for Backend interface consistency, but NOTE: compose-go's
// LoadWithContext propagates ctx to subroutines but does NOT actively check
// ctx.Err() in its main loop. With our Skip* options set, the parse is
// CPU-bound on in-memory bytes (<100ms typically). ctx-cancel will not
// abort the parse mid-flight.
func loadComposeProject(ctx context.Context, bytes []byte, filename, projectName string) (*composetypes.Project, error) {
	return composeloader.LoadWithContext(ctx,
		composetypes.ConfigDetails{
			ConfigFiles: []composetypes.ConfigFile{{Content: bytes, Filename: filename}},
		},
		func(opts *composeloader.Options) {
			opts.SkipValidation = false
			opts.SkipConsistencyCheck = true
			opts.SkipExtends = true
			opts.SkipInclude = true
			opts.SkipResolveEnvironment = true
			opts.SetProjectName(projectName, true)
		},
	)
}

// newComposeCli constructs a docker/cli command.Cli — the type
// docker/compose/v2 requires for compose.NewComposeService. Reads
// DOCKER_HOST / DOCKER_TLS_VERIFY etc. from the environment via FromEnv-
// equivalent flags; suppresses the CLI's stdout/stderr by routing to
// io.Discard.
func newComposeCli() (command.Cli, error) {
	cli, err := command.NewDockerCli(
		command.WithOutputStream(io.Discard),
		command.WithErrorStream(io.Discard),
	)
	if err != nil {
		return nil, fmt.Errorf("NewDockerCli: %w", err)
	}
	if err := cli.Initialize(&cliflags.ClientOptions{}); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	return cli, nil
}
