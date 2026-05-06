package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/valbaudo/awf/obs"
	"github.com/valbaudo/awf/state"
)

func printInspectUsage(w io.Writer) {
	fprintln(w, "usage: awf inspect <run-id> [--state-dir <dir>] [--fold <status,...>] [--depth <n>] [--output text|json]")
	fprintln(w, "")
	fprintln(w, "  render a run's addressing tree as a fold-by-status text tree.")
	fprintln(w, "  ok subtrees collapse by default; failed / rejected / incomplete subtrees expand.")
	fprintln(w, "  --fold <statuses>  comma list of node outcomes to collapse (default: ok)")
	fprintln(w, "  --depth <n>        max tree depth to render (default: unlimited)")
	fprintln(w, "  --output <fmt>     text (default) or json (the obs.Span projection)")
	fprintln(w, "  --state-dir <dir>  base directory for runs/ (default: ./.awf)")
	fprintln(w, "")
	fprintln(w, "  NOTE: AWF does not offer Temporal-style deterministic replay; resume folds")
	fprintln(w, "  the log and re-runs only the uncommitted frontier (no author-code determinism).")
}

func cliInspect(args []string, stdout, stderr io.Writer) int {
	fs0 := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs0.SetOutput(io.Discard)
	fs0.Usage = func() {}
	stateDir := fs0.String("state-dir", ".awf", "base directory for runs/")
	foldArg := fs0.String("fold", "ok", "comma list of statuses to collapse")
	depth := fs0.Int("depth", -1, "max tree depth (-1 = unlimited)")
	output := fs0.String("output", "text", "output format: text or json")
	// Extract the positional run-id before parsing flags.
	// Go's flag package stops at the first non-flag argument, so "r1 --flag"
	// would leave --flag unparsed if we don't separate them first.
	if len(args) == 0 {
		printInspectUsage(stderr)
		return ExitUsage
	}
	if strings.HasPrefix(args[0], "-") {
		// No run-id first: let flag parse it so -h/--help yields ErrHelp → ExitOK
		// (consistent with pause/cancel); any other leading flag is "missing run-id".
		if err := fs0.Parse(args); errors.Is(err, flag.ErrHelp) {
			printInspectUsage(stdout)
			return ExitOK
		}
		printInspectUsage(stderr)
		return ExitUsage
	}
	runID := args[0]
	if err := fs0.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printInspectUsage(stdout)
			return ExitOK
		}
		fprintf(stderr, "awf inspect: %v\n", err)
		printInspectUsage(stderr)
		return ExitUsage
	}
	if fs0.NArg() != 0 {
		printInspectUsage(stderr)
		return ExitUsage
	}
	if *output != "text" && *output != "json" {
		fprintf(stderr, "awf inspect: unknown --output %q (want text or json)\n", *output)
		return ExitUsage
	}

	logPath := filepath.Join(*stateDir, "runs", runID, "log")
	events, err := state.FoldFile(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fprintf(stderr, "awf inspect: no run with id %q at %q\n", runID, logPath)
		} else {
			fprintf(stderr, "awf inspect: fold log %q: %v\n", logPath, err)
		}
		return ExitUsage
	}
	spans, err := obs.Project(events, nil)
	if err != nil {
		fprintf(stderr, "awf inspect: project log: %v\n", err)
		return ExitUsage
	}

	if *output == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(spans); err != nil {
			fprintf(stderr, "awf inspect: json encode: %v\n", err)
			return ExitUsage
		}
		return ExitOK
	}

	foldSet := map[string]bool{}
	for _, s := range strings.Split(*foldArg, ",") {
		if s = strings.TrimSpace(s); s != "" {
			foldSet[s] = true
		}
	}
	renderTree(stdout, spans, foldSet, *depth)
	return ExitOK
}

// renderTree prints the span forest as an indented, fold-by-status text tree.
// Children are indexed by ParentPath; the run root is the span with Path "".
func renderTree(w io.Writer, spans []obs.Span, foldSet map[string]bool, maxDepth int) {
	byPath := map[string]obs.Span{}
	children := map[string][]obs.Span{}
	for _, s := range spans {
		byPath[s.Path] = s
		if s.Path != "" {
			children[s.ParentPath] = append(children[s.ParentPath], s)
		}
	}
	for p := range children {
		kids := children[p]
		sort.Slice(kids, func(i, j int) bool { return kids[i].Path < kids[j].Path })
		children[p] = kids
	}
	// notable[path] = this subtree contains a failure / incomplete node worth expanding.
	notable := map[string]bool{}
	var markNotable func(path string) bool
	markNotable = func(path string) bool {
		s := byPath[path]
		self := s.Status == obs.StatusError || s.Pending || isNotableOutcome(s)
		any := self
		for _, c := range children[path] {
			if markNotable(c.Path) {
				any = true
			}
		}
		notable[path] = any
		return any
	}
	if _, ok := byPath[""]; ok {
		markNotable("")
	}

	var render func(path string, depth int)
	render = func(path string, depth int) {
		s := byPath[path]
		fprintf(w, "%s%s\n", strings.Repeat("  ", depth), nodeLine(s))
		if maxDepth >= 0 && depth >= maxDepth {
			return
		}
		// Collapse: stop recursing if this node's status is in the fold set AND
		// nothing notable lives below it. NEVER collapse the root (depth 0) — a
		// fully-successful run must still show its top-level nodes, otherwise
		// `awf inspect` of an all-ok run prints just "run … collapsed", which is
		// useless. Ok subtrees collapse only at depth ≥ 1.
		if depth > 0 && foldSet[nodeStatusToken(s)] && !subtreeNotableBelow(children, notable, path) {
			if len(children[path]) > 0 {
				fprintf(w, "%s… (%d collapsed)\n", strings.Repeat("  ", depth+1), len(children[path]))
			}
			return
		}
		for _, c := range children[path] {
			render(c.Path, depth+1)
		}
	}
	if _, ok := byPath[""]; ok {
		render("", 0)
	}
}

func subtreeNotableBelow(children map[string][]obs.Span, notable map[string]bool, path string) bool {
	for _, c := range children[path] {
		if notable[c.Path] {
			return true
		}
	}
	return false
}

func isNotableOutcome(s obs.Span) bool {
	switch outcomeAttr(s) {
	case "rejected", "retryable_failure", "permanent_failure", "incomplete":
		return true
	}
	return false
}

func outcomeAttr(s obs.Span) string {
	if v, ok := s.Attributes[obs.AttrNodeOutcome].(string); ok {
		return v
	}
	return ""
}

// nodeStatusToken is the short status used for fold-matching and display.
func nodeStatusToken(s obs.Span) string {
	if s.Pending {
		return "incomplete"
	}
	if s.Status == obs.StatusError {
		if o := outcomeAttr(s); o != "" {
			return o
		}
		return "failed"
	}
	if o := outcomeAttr(s); o != "" {
		return o
	}
	return "ok"
}

// nodeLine is one rendered tree line: name/id, kind, status, optional cost.
func nodeLine(s obs.Span) string {
	name := s.Name
	if name == "" {
		name = "run"
	}
	// For the run root (Path ""), prefer the run-id attribute over the generic
	// "run" name so the output is unambiguous to the caller.
	if s.Path == "" {
		if id, ok := s.Attributes[obs.AttrRunID].(string); ok && id != "" {
			name = id
		}
	}
	parts := []string{name}
	if s.Kind != "" {
		parts = append(parts, s.Kind)
	}
	parts = append(parts, nodeStatusToken(s))
	if c, ok := s.Attributes[obs.AttrCostUSD].(float64); ok {
		parts = append(parts, fmt.Sprintf("$%.4f", c))
	}
	return strings.Join(parts, "  ")
}
