// Package reverse turns normalized change events into inverse SQL:
// INSERT becomes DELETE, DELETE becomes INSERT of the old row, and UPDATE is
// rewritten back to the prior row image.
//
// The engine proposes SQL for a human to review; it does not execute anything
// itself. Conflict detection (a later statement touched the same row) lives here
// too, as warnings rather than automatic resolution.
package reverse
