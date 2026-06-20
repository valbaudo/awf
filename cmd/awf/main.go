// Command awf is the AWF runtime CLI entry point. All real logic lives in the cli package;
// this binary is a four-line wrapper that hands os.Args + the standard writers to cli.Run
// and exits with the returned code.
//
// WS-5: Before delegating to cli.Run, maybeSandboxTrampoline() is called to
// intercept the __sandbox re-exec subcommand used by the Linux go-landlock
// trampoline launcher. On non-Linux platforms and for normal invocations this
// call is a no-op.
package main

import (
	"os"

	"github.com/valbaudo/awf/cli"
)

func main() {
	// __sandbox trampoline: must run before any CLI parsing and before
	// constructing any Runner. On Linux, if os.Args[1] == "__sandbox",
	// this function applies Landlock FS restrictions and syscall.Exec's
	// /bin/sh — it never returns. On all other platforms and for normal
	// invocations it is a no-op.
	maybeSandboxTrampoline()

	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
