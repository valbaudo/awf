package cli

import (
	"flag"
	"io"
	"net/http"
	"os/exec"
	"runtime"

	"github.com/valbaudo/awf/loader"
	"github.com/valbaudo/awf/ui"
)

func printUIUsage(w io.Writer) {
	fprintln(w, "usage: awf ui <path> [--state-dir <dir>] [--port <n>] [--open]")
	fprintln(w, "")
	fprintln(w, "  serve a local web UI that renders the workflow graph and overlays run state.")
	fprintln(w, "  open the printed URL in a browser; pick a run to see its state, Refresh to update.")
	fprintln(w, "  --state-dir <dir>  base directory for runs/ (default: ./.awf)")
	fprintln(w, "  --port <n>         port to bind on 127.0.0.1 (default: 0 = ephemeral)")
	fprintln(w, "  --open             open the URL in your default browser (best-effort)")
	fprintln(w, "")
	fprintln(w, "  read-only; binds 127.0.0.1 only (no auth, no remote exposure).")
}

// cliUI runs `awf ui <path> [--state-dir <dir>] [--port <n>] [--open]`. It blocks
// serving until interrupted. Returns ExitUsage on a bad path / load / bind failure.
func cliUI(args []string, stdout, stderr io.Writer) int {
	fs0 := flag.NewFlagSet("ui", flag.ContinueOnError)
	fs0.SetOutput(io.Discard)
	fs0.Usage = func() {}
	stateDir := fs0.String("state-dir", ".awf", "base directory for runs/")
	port := fs0.Int("port", 0, "port to bind on 127.0.0.1 (0 = ephemeral)")
	open := fs0.Bool("open", false, "open the URL in the default browser")
	path, code, ok := parseRunIDFirst(fs0, args, "awf ui", printUIUsage, stdout, stderr)
	if !ok {
		return code
	}

	ld, err := loader.Load(path)
	if err != nil {
		fprintf(stderr, "awf ui: %v\n", err)
		return ExitUsage
	}
	digest, err := ld.ComputeDigest()
	if err != nil {
		fprintf(stderr, "awf ui: compute digest: %v\n", err)
		return ExitUsage
	}

	ln, err := ui.Listen(*port)
	if err != nil {
		fprintf(stderr, "awf ui: bind 127.0.0.1:%d: %v\n", *port, err)
		return ExitUsage
	}
	url := "http://" + ln.Addr().String()
	fprintf(stdout, "awf ui: serving %s on %s\n", path, url)

	srv := ui.NewLoaded(ld, digest, *stateDir)
	if *open {
		openBrowser(url) // best-effort; the URL is already printed
	}
	if err := http.Serve(ln, srv.Handler()); err != nil {
		fprintf(stderr, "awf ui: serve: %v\n", err)
		return ExitRunFailed
	}
	return ExitOK
}

// openBrowser best-effort launches the platform browser. Failure is non-fatal: the URL
// is already on stdout.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "cmd", []string{"/c", "start"}
	default:
		cmd = "xdg-open"
	}
	_ = exec.Command(cmd, append(args, url)...).Start()
}
