package cli

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/spf13/pflag"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
)

// validateResult is the JSON projection of one `awf validate` invocation. Both the outer
// fields and the inner []ir.Diagnostic use lowercase JSON tags (CLI convention), so the whole
// document reads with consistent casing — e.g. `awf validate --output json | jq
// '.diagnostics[].code'` works without case-special-casing. The Diagnostic tags live in
// ir/diagnostic.go; the 35 ir/testdata/invalid/*.golden fixtures (plus the cli golden) encode
// the lowercase contract.
type validateResult struct {
	Path        string          `json:"path"`
	Digest      string          `json:"digest,omitempty"`
	Diagnostics []ir.Diagnostic `json:"diagnostics"`
}

// printValidateUsage writes the validate-subcommand usage line. Pulled out so help (stdout,
// exit 0) and errors (stderr, exit 2) can share the same wording without drift.
func printValidateUsage(w io.Writer) {
	fprintln(w, "usage: awf validate [-o|--output text|json] <path>")
}

// cliValidate runs `awf validate [-o|--output text|json] <path>`. Returns:
//   - ExitOK on clean validation (zero error-severity diagnostics; warnings are OK).
//   - ExitInvalid on ≥1 error-severity diagnostic.
//   - ExitUsage on missing/extra args, unknown --output, or loader-stage failure.
//
// Loader failures (unreadable file, bad YAML, path escape, missing compose) print to stderr
// and return ExitUsage — they aren't validation diagnostics. ir.Validate diagnostics print
// to stdout in the chosen format. The digest is computed from the parsed IR + compose bytes
// and printed even when validation has errors (it's the canonical hash, not a "valid" stamp;
// operators benefit from seeing what they're hashing).
func cliValidate(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("validate", pflag.ContinueOnError)
	// Discard the FlagSet's auto-emitted output (the "Usage of validate:" boilerplate and the
	// "flag provided but not defined: -bogus" error message). We own the output channels:
	// help goes to stdout, errors to stderr — both via printValidateUsage so the wording
	// can't drift between the two paths.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {} // no-op; printValidateUsage is the canonical writer.
	// --output (with -o shorthand) is canonical; --format is a hidden, deprecated
	// alias bound to the SAME target, so either name sets `output` and last-flag
	// wins. The deprecation notice is printed by us (after parse) rather than via
	// pflag's MarkDeprecated, because the FlagSet output is discarded.
	var output string
	fs.StringVarP(&output, "output", "o", "text", "output format: text or json")
	fs.StringVar(&output, "format", "text", "deprecated alias for --output")
	_ = fs.MarkHidden("format")
	err := fs.Parse(args)
	if errors.Is(err, pflag.ErrHelp) {
		// pflag returns ErrHelp for `-h`, `--help`, AND `-help` (single dash, full
		// word — its first char `h` triggers the help shorthand). Catching the
		// sentinel handles all three; a string-search would miss `-help`. Help goes
		// to stdout so `awf validate -h | grep …` works.
		printValidateUsage(stdout)
		return ExitOK
	}
	if err != nil {
		fprintf(stderr, "awf validate: %v\n", err)
		printValidateUsage(stderr)
		return ExitUsage
	}
	if fs.NArg() != 1 {
		printValidateUsage(stderr)
		return ExitUsage
	}
	path := fs.Arg(0)
	if fs.Changed("format") {
		// --format is the deprecated alias for --output; warn on stderr so that a
		// `... --format json | jq` pipe still receives clean JSON on stdout.
		fprintf(stderr, "awf validate: --format is deprecated; use --output\n")
	}

	ld, loadErr := loader.Load(path)
	if loadErr != nil {
		var le *loader.LoadError
		if errors.As(loadErr, &le) {
			msg := le.Message
			if msg == "" && le.Err != nil {
				msg = le.Err.Error()
			}
			diags := []ir.Diagnostic{{
				Severity: ir.Error,
				Source:   le.Source,
				Path:     le.Path,
				Code:     le.Code,
				Message:  msg,
			}}
			switch output {
			case "json":
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(validateResult{Path: path, Diagnostics: diags}); err != nil {
					fprintf(stderr, "awf validate: json encode: %v\n", err)
					return ExitUsage
				}
			case "text":
				printTextResult(stdout, path, "", diags)
			default:
				fprintf(stderr, "awf validate: unknown --output %q (want text or json)\n", output)
				return ExitUsage
			}
			return ExitInvalid
		}
		fprintf(stderr, "awf validate: %v\n", loadErr)
		return ExitUsage
	}

	diags := ir.Validate(ld)

	// Compute the canonical digest (includes the spec §E compose-fold via ir/digest.go).
	// If ComputeDigest fails (highly unlikely once the workflow has parsed), surface a
	// warning to stderr and proceed with an empty digest field — validation diagnostics are
	// the primary output and shouldn't be lost to a digest-pipeline edge case.
	digest, digestErr := ld.ComputeDigest()
	if digestErr != nil {
		fprintf(stderr, "awf validate: warning: digest unavailable: %v\n", digestErr)
		digest = ""
	}

	switch output {
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(validateResult{Path: path, Digest: digest, Diagnostics: diags}); err != nil {
			fprintf(stderr, "awf validate: json encode: %v\n", err)
			return ExitUsage
		}
	case "text":
		printTextResult(stdout, path, digest, diags)
	default:
		fprintf(stderr, "awf validate: unknown --output %q (want text or json)\n", output)
		return ExitUsage
	}

	if ir.HasErrors(diags) {
		return ExitInvalid
	}
	return ExitOK
}

// printTextResult renders the human-readable validate report. Format (one diagnostic per line,
// d.String()'s "<severity> <code> at <path>: <message>" shape from slice 1.4):
//
//	<path>: ok
//	digest: awf-d1:sha256:<hex>
//
// or, on issues:
//
//	<path>: 1 error, 2 warnings
//
//	  error AWF1004 at graph[2]: step id is not unique
//	  warning AWF2002 at graph[0].output_schema: ...
//
//	digest: awf-d1:sha256:<hex>
//
// Stable across runs because slice 1.4's ir.Validate already sorts diagnostics by (Code, Path,
// Message) before returning.
func printTextResult(w io.Writer, path, digest string, diags []ir.Diagnostic) {
	errs, warns := 0, 0
	for _, d := range diags {
		switch d.Severity {
		case ir.Error:
			errs++
		case ir.Warning:
			warns++
		}
	}

	switch {
	case errs == 0 && warns == 0:
		fprintf(w, "%s: ok\n", path)
	case errs > 0 && warns > 0:
		fprintf(w, "%s: %d %s, %d %s\n", path, errs, plural(errs, "error"), warns, plural(warns, "warning"))
	case errs > 0:
		fprintf(w, "%s: %d %s\n", path, errs, plural(errs, "error"))
	default:
		fprintf(w, "%s: %d %s\n", path, warns, plural(warns, "warning"))
	}

	if len(diags) > 0 {
		fprintln(w, "")
		for _, d := range diags {
			fprintf(w, "  %s\n", d) // Diagnostic.String() includes severity + code + path + message
		}
	}

	if digest != "" {
		fprintln(w, "")
		fprintf(w, "digest: %s\n", digest)
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
