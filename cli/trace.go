package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"path/filepath"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/valbaudo/awf/obs"
	"github.com/valbaudo/awf/state"
)

func printTraceUsage(w io.Writer) {
	fprintln(w, "usage: awf trace <run-id> [--state-dir <dir>] [--otlp <endpoint>] [--capture-content] [--output otel|json]")
	fprintln(w, "")
	fprintln(w, "  project a run's log into OpenTelemetry spans and export them.")
	fprintln(w, "  default            local stdout span exporter (zero-infra)")
	fprintln(w, "  --otlp <host:port> export to an OTLP/HTTP collector (plaintext)")
	fprintln(w, "  --output json      dump the obs.Span projection as JSON (downloadable) instead of exporting")
	fprintln(w, "  --capture-content  attach agent I/O + typed-output/stdout (opt-in, default OFF)")
	fprintln(w, "  --state-dir <dir>  base directory for runs/ and blobs/ (default: ./.awf)")
	fprintln(w, "")
	fprintln(w, "  WARNING: --capture-content with --otlp transmits agent I/O (prompts, agent")
	fprintln(w, "  output, stdout) to the collector — including any target/exploit detail or")
	fprintln(w, "  secrets embedded in prompts. Content capture is OFF by default.")
	fprintln(w, "")
	fprintln(w, "  NOTE: AWF does not offer Temporal-style deterministic replay; resume folds")
	fprintln(w, "  the log and re-runs only the uncommitted frontier (no author-code determinism).")
}

func cliTrace(args []string, stdout, stderr io.Writer) int {
	fs0 := flag.NewFlagSet("trace", flag.ContinueOnError)
	fs0.SetOutput(io.Discard)
	fs0.Usage = func() {}
	stateDir := fs0.String("state-dir", ".awf", "base directory for runs/ and blobs/")
	otlp := fs0.String("otlp", "", "OTLP/HTTP endpoint host:port")
	output := fs0.String("output", "otel", "output format: otel or json")
	capture := fs0.Bool("capture-content", false, "attach agent I/O + typed-output/stdout content")
	runID, code, ok := parseRunIDFirst(fs0, args, "awf trace", printTraceUsage, stdout, stderr)
	if !ok {
		return code
	}
	if *output != "otel" && *output != "json" {
		fprintf(stderr, "awf trace: unknown --output %q (want otel or json)\n", *output)
		return ExitUsage
	}

	logPath := filepath.Join(*stateDir, "runs", runID, "log")
	events, err := state.FoldFile(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fprintf(stderr, "awf trace: no run with id %q at %q\n", runID, logPath)
		} else {
			fprintf(stderr, "awf trace: fold log %q: %v\n", logPath, err)
		}
		return ExitUsage
	}

	opts := obs.ProjectOptions{CaptureContent: *capture}
	var blobs state.Blobs
	if *capture {
		fb, berr := state.OpenBlobs(filepath.Join(*stateDir, "blobs"))
		if berr != nil {
			fprintf(stderr, "awf trace: open blobs: %v\n", berr)
			return ExitUsage
		}
		blobs = fb
	}
	spans, err := obs.ProjectWithOptions(events, blobs, opts)
	if err != nil {
		fprintf(stderr, "awf trace: project log: %v\n", err)
		return ExitUsage
	}

	if *output == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(spans); err != nil {
			fprintf(stderr, "awf trace: json encode: %v\n", err)
			return ExitUsage
		}
		return ExitOK
	}

	// Content + a network collector exfiltrates agent I/O — warn explicitly.
	if *capture && *otlp != "" {
		fprintf(stderr, "awf trace: warning: --capture-content with --otlp transmits agent I/O to %s\n", *otlp)
	}

	ctx := context.Background()
	// Content capture attaches one span event per agent.event; lift the SDK's
	// default 128-event-per-span cap (EventCountLimit) so they aren't silently
	// dropped. Non-capture uses the SDK defaults unchanged.
	limits := sdktrace.NewSpanLimits()
	if *capture {
		limits.EventCountLimit = -1
		limits.AttributePerEventCountLimit = -1
	}
	var tp *sdktrace.TracerProvider
	var perr error
	if *otlp != "" {
		tp, perr = obs.NewOTLPProviderWithLimits(ctx, *otlp, limits)
	} else {
		tp, perr = obs.NewStdoutProviderWithLimits(stdout, limits)
	}
	if perr != nil {
		fprintf(stderr, "awf trace: build exporter: %v\n", perr)
		return ExitUsage
	}
	defer func() { _ = tp.Shutdown(ctx) }()
	if err := obs.Export(ctx, spans, tp); err != nil {
		fprintf(stderr, "awf trace: export: %v\n", err)
		return ExitUsage
	}
	return ExitOK
}
