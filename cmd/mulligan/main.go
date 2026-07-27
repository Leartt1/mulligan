// Command mulligan is the entry point for the Mulligan undo console.
//
// It is a work in progress — see PLAN.md for the design and roadmap.
package main

import "fmt"

const version = "0.0.0-dev"

func main() {
	fmt.Printf("mulligan %s — Ctrl-Z for the database you already have\n", version)
	fmt.Println("not implemented yet — see PLAN.md")
}
