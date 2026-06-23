package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"

	"github.com/spf13/pflag"
)

// version is the release version string. A tagged build overrides it at link time:
//
//	go build -ldflags "-X github.com/valbaudo/awf/cli.version=v1.2.3"
//
// Left "(devel)" for an unstamped build.
var version = "(devel)"

// versionInfo is the resolved build identity. JSON tags are lowercase + never omitempty: the
// `awf version -o json` contract is a fixed five-key object, empty strings where unknown.
type versionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Dirty     bool   `json:"dirty"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
}

// resolveVersion folds the link-time version + the VCS metadata Go bakes into a `go build`
// (debug.ReadBuildInfo's vcs.* settings) into a versionInfo. It never errors: when ok is false
// or bi is nil (e.g. a `go run` binary), only ver and goVersion survive — the VCS fields stay
// empty, which the text/JSON renderers fold into their "unknown" forms. Commit is kept full
// here (the JSON contract carries the untruncated revision); text() does the 12-char display
// truncation.
func resolveVersion(ver, goVersion string, bi *debug.BuildInfo, ok bool) versionInfo {
	info := versionInfo{Version: ver, GoVersion: goVersion}
	if !ok || bi == nil {
		return info
	}
	// A `go install module@vX.Y.Z` build never runs the -ldflags stamp, so ver is still the
	// "(devel)" default — but the module version is recorded in BuildInfo.Main.Version. Adopt it
	// so installed binaries report their tag. A real linker stamp (the release-tarball path) wins,
	// and an absent/"(devel)" Main.Version (go run / go build in a module) leaves "(devel)" intact.
	if info.Version == "(devel)" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		info.Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Commit = s.Value
		case "vcs.time":
			info.BuildTime = s.Value
		case "vcs.modified":
			info.Dirty = s.Value == "true"
		}
	}
	return info
}

// currentVersion resolves the running binary's identity. runtime.Version() is the go-version
// source so the field is populated even when ReadBuildInfo() reports ok=false.
func currentVersion() versionInfo {
	bi, ok := debug.ReadBuildInfo()
	return resolveVersion(version, runtime.Version(), bi, ok)
}

// text renders the one-line human form:
//
//	awf <version> (commit <12-char-sha>[+dirty], built <build-time>, <go-version>)
//
// With no commit (go run / no VCS keys) it collapses to the unknown form rather than emit a
// half-populated line:
//
//	awf <version> (commit unknown, <go-version>)
func (v versionInfo) text() string {
	if v.Commit == "" {
		return fmt.Sprintf("awf %s (commit unknown, %s)", v.Version, v.GoVersion)
	}
	sha := v.Commit
	if len(sha) > 12 {
		sha = sha[:12]
	}
	dirty := ""
	if v.Dirty {
		dirty = "+dirty"
	}
	return fmt.Sprintf("awf %s (commit %s%s, built %s, %s)", v.Version, sha, dirty, v.BuildTime, v.GoVersion)
}

// printVersionUsage writes the version-subcommand usage line (shared by help + error paths).
func printVersionUsage(w io.Writer) {
	fprintln(w, "usage: awf version [-o|--output text|json]")
}

// cliVersion runs `awf version [-o|--output text|json]` (and, via cli.go, the top-level
// `awf --version`, which routes here with nil args → the default text form). Always ExitOK on
// a clean parse — resolution never fails; an unknown --output or stray positional is ExitUsage.
func cliVersion(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("version", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var output string
	fs.StringVarP(&output, "output", "o", "text", "output format: text or json")
	err := fs.Parse(args)
	if errors.Is(err, pflag.ErrHelp) {
		printVersionUsage(stdout)
		return ExitOK
	}
	if err != nil {
		fprintf(stderr, "awf version: %v\n", err)
		printVersionUsage(stderr)
		return ExitUsage
	}
	if fs.NArg() != 0 {
		printVersionUsage(stderr)
		return ExitUsage
	}

	info := currentVersion()
	switch output {
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(info); err != nil {
			fprintf(stderr, "awf version: json encode: %v\n", err)
			return ExitUsage
		}
	case "text":
		fprintln(stdout, info.text())
	default:
		fprintf(stderr, "awf version: unknown --output %q (want text or json)\n", output)
		return ExitUsage
	}
	return ExitOK
}
