package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"io"

	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/loader"
)

// validateResult is the JSON projection of one `awf validate` invocation. Outer fields use
// lowercase JSON tags (CLI convention). The inner []ir.Diagnostic retains the Go-default
// capitalized field names — slice 1.4 explicitly designed Diagnostic to marshal that way
// (see ir/diagnostic.go's "String renders ... The JSON projection marshals the struct
// directly with the default Go field names"), and its golden tests (ir/testdata/invalid/
// *.golden, ~25 files) encode that contract. Changing it would force regenerating all those
// goldens — out of scope for this slice. If the asymmetry ever becomes a real ergonomics
// complaint, it lands as a dedicated follow-up.
type validateResult struct {
	Path        string          `json:"path"`
	Digest      string          `json:"digest,omitempty"`
	Diagnostics []ir.Diagnostic `json:"diagnostics"`
}

// printValidateUsage writes the validate-subcommand usage line. Pulled out so help (stdout,
// exit 0) and errors (stderr, exit 2) can share the same wording without drift.
func printValidateUsage(w io.Writer) {
	fprintln(w, "usage: awf validate [--format text|json] <path>")
}

// cliValidate runs `awf validate [--format text|json] <path>`. Returns:
//   - ExitOK on clean validation (zero error-severity diagnostics; warnings are OK).
//   - ExitInvalid on ≥1 error-severity diagnostic.
//   - ExitUsage on missing/extra args, unknown --format, or loader-stage failure.
//
// Loader failures (unreadable file, bad YAML, path escape, missing compose) print to stderr
// and return ExitUsage — they aren't validation diagnostics. ir.Validate diagnostics print
// to stdout in the chosen format. The digest is computed from the parsed IR + compose bytes
// and printed even when validation has errors (it's the canonical hash, not a "valid" stamp;
// operators benefit from seeing what they're hashing).
func cliValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	// Discard the FlagSet's auto-emitted output (the "Usage of validate:" boilerplate and the
	// "flag provided but not defined: -bogus" error message). We own the output channels:
	// help goes to stdout, errors to stderr — both via printValidateUsage so the wording
	// can't drift between the two paths.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {} // no-op; printValidateUsage is the canonical writer.
	format := fs.String("format", "text", "output format: text or json")
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		// Go's flag package returns flag.ErrHelp for `-h`, `--help`, AND `-help` (single
		// dash, full word). Catching the sentinel handles all three; a string-search would
		// miss `-help`. Help goes to stdout so `awf validate -h | grep …` works.
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
			switch *format {
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
				fprintf(stderr, "awf validate: unknown --format %q (want text or json)\n", *format)
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

	switch *format {
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
		fprintf(stderr, "awf validate: unknown --format %q (want text or json)\n", *format)
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
