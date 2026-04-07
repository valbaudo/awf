// Command awf is the AWF runtime CLI entry point. All real logic lives in the cli package;
// this binary is a four-line wrapper that hands os.Args + the standard writers to cli.Run
// and exits with the returned code.
package main

import (
	"os"

	"github.com/valbaudo/awf/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
