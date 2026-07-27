// Command mulligan generates SQL that undoes recent logged database changes.
//
// It proposes; it does not execute. See PLAN.md for the design and roadmap.
package main

import (
	"os"

	"github.com/learttytyri/mulligan/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
