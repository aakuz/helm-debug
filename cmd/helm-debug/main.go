// Command helm-debug debugs Helm releases by reading their records straight from the cluster.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/aakuz/helm-debug/internal/cli"
)

func main() {
	err := cli.NewRootCommand(cli.NewOptions()).Execute()
	switch {
	case err == nil:
	case errors.Is(err, cli.ErrProblems):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
