package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/signal"
	"github.com/valbaudo/awf/state"
	"github.com/valbaudo/awf/template"
)

const runtimeComposeTeardownGrace = 30 * time.Second

func runCompose(
	ctx context.Context,
	n *ir.Compose,
	path string,
	wf *ir.Workflow,
	runstate *RunState,
	dispatcher Dispatcher,
	log state.Log,
	blobs state.Blobs,
	clk clock.Clock,
	tap io.Writer,
	broker *signal.Broker,
) (Outcome, error) {
	return runComposeWithContext(ctx, n, path, interpreterContext{
		wf: wf, runstate: runstate, dispatcher: dispatcher, log: log, blobs: blobs, clk: clk, tap: tap, broker: broker,
	})
}

func runComposeWithContext(
	ctx context.Context,
	n *ir.Compose,
	path string,
	ictx interpreterContext,
) (Outcome, error) {
	ld, ok := ictx.dispatcher.(*LocalDispatcher)
	if !ok {
		return "", fmt.Errorf("engine.runCompose: compose at %q requires *LocalDispatcher for runtime compose promotion (got %T)", path, ictx.dispatcher)
	}

	scope := ictx.scope(path)
	composeBytes, err := resolveArtifactBytes(n.From, scope, ictx.wf, ictx.blobs)
	if err != nil {
		if errors.Is(err, errArtifactFetch) {
			return "", fmt.Errorf("engine.Run: promote compose at %q: %w", path, err)
		}
		return failStep(ictx.log, path, OutcomePermanentFailure, fmt.Errorf("runtime compose from: %w", err))
	}

	service, err := template.Substitute(string(n.Service), scope)
	if err != nil {
		return failStep(ictx.log, path, OutcomePermanentFailure, fmt.Errorf("runtime compose service: %w", err))
	}
	service = strings.TrimSpace(service)
	if service == "" {
		return failStep(ictx.log, path, OutcomePermanentFailure, fmt.Errorf("runtime compose service rendered empty"))
	}

	requiredServices := append([]string{service}, composeServiceOverrides(n.As, n.Body)...)
	if errs := ir.ValidateComposeBytes(runtimeComposeFilename(path, n.As), composeBytes, requiredServices...); len(errs) > 0 {
		return failStep(ictx.log, path, OutcomePermanentFailure, formatComposeValidationErrors(errs))
	}

	spec := container.ContainerSpec{
		Name:        runtimeComposeName(path, n.As),
		Compose:     composeBytes,
		ComposePath: runtimeComposeFilename(path, n.As),
		Service:     service,
	}
	h, err := ld.Backend.Create(ctx, spec)
	if err != nil {
		return failStep(ictx.log, path, OutcomeRetryableFailure, fmt.Errorf("runtime compose create: %w", err))
	}

	scopedDispatcher := ld.WithItemHandle(n.As, h)
	bodyCtx := ictx
	bodyCtx.dispatcher = scopedDispatcher
	bodyOC, bodyErr := interpNodes(ctx, n.Body, path+".body", bodyCtx)

	cleanupCtx, cancel := context.WithTimeout(context.Background(), runtimeComposeTeardownGrace)
	destroyErr := ld.Backend.Destroy(cleanupCtx, h)
	cancel()
	if destroyErr != nil {
		destroyErr = fmt.Errorf("runtime compose destroy: %w", destroyErr)
		if bodyErr != nil || bodyOC != OutcomeOK {
			return bodyOC, errors.Join(bodyErr, destroyErr)
		}
		return failStep(ictx.log, path, OutcomeRetryableFailure, destroyErr)
	}
	return bodyOC, bodyErr
}

func resolveArtifactBytes(ref string, scope *Scope, wf *ir.Workflow, blobs state.Blobs) ([]byte, error) {
	id, name, ok := template.ParseArtifactRef(ref)
	if !ok {
		return nil, fmt.Errorf("%q: expected step.<id>.files.<name>", ref)
	}
	cas, err := resolveNamedArtifactRef(scope, wf, id, name)
	if err != nil {
		return nil, err
	}
	b, err := blobs.Get(cas)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", errArtifactFetch, err)
	}
	return b, nil
}

func composeServiceOverrides(as string, body ir.NodeList) []string {
	seen := map[string]bool{}
	var out []string
	ir.WalkNodes(body, "", func(n ir.Node, _ string) {
		var ref string
		switch s := n.(type) {
		case *ir.CodeStep:
			ref = s.Container
		case *ir.AgentStep:
			ref = s.Container
		case *ir.Map:
			if s.Reduce != nil && s.Reduce.IsRun() {
				ref = s.Reduce.Container
			}
		default:
			return
		}
		bare, svc := SplitContainerRef(ref)
		if bare != as || svc == "" || seen[svc] {
			return
		}
		seen[svc] = true
		out = append(out, svc)
	})
	sort.Strings(out)
	return out
}

func formatComposeValidationErrors(errs []ir.ComposeValidationError) error {
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		parts = append(parts, fmt.Sprintf("%s: %s", err.Code, err.Message))
	}
	return fmt.Errorf("runtime compose validation failed: %s", strings.Join(parts, "; "))
}

var runtimeComposeNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func runtimeComposeName(path, as string) string {
	raw := path + "-" + as
	name := runtimeComposeNameUnsafe.ReplaceAllString(raw, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		return "compose"
	}
	return name
}

func runtimeComposeFilename(path, as string) string {
	return runtimeComposeName(path, as) + ".yml"
}
