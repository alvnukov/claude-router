// Package cli holds the subcommands of the localrouter binary: the table
// that picks one and the commands that manage the router as a system
// service.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// Command runs one subcommand with the arguments after its name.
type Command func(ctx context.Context, args []string, stdout, stderr io.Writer) error

// Entry names a subcommand.
type Entry struct {
	Name string
	Run  Command
}

// UsageError is a command line the command does not accept; it exits 2.
type UsageError string

func (e UsageError) Error() string { return string(e) }

// Dispatch runs the subcommand args[0] names, or the first entry of table
// when there are no arguments: the service manager starts the router with
// none. It returns the exit code.
func Dispatch(ctx context.Context, table []Entry, args []string, stdout, stderr io.Writer) int {
	name, rest := table[0].Name, []string{}
	if len(args) > 0 {
		name, rest = args[0], args[1:]
	}
	for _, entry := range table {
		if entry.Name != name {
			continue
		}
		err := entry.Run(ctx, rest, stdout, stderr)
		var usage UsageError
		switch {
		case err == nil, errors.Is(err, flag.ErrHelp):
			return 0
		case errors.As(err, &usage):
			fmt.Fprintln(stderr, err)
			return 2
		default:
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	names := make([]string, len(table))
	for i, entry := range table {
		names[i] = entry.Name
	}
	fmt.Fprintf(stderr, "usage: localrouter {%s}\n", strings.Join(names, "|"))
	return 2
}
