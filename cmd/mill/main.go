// Command mill is the single entry point to mill: the CLI and the service.
package main

import (
	"os"

	"github.com/ivy/mill/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
