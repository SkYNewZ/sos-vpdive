// Command sos-vpdive runs the support desk of a diving club using VPDive.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	_ "time/tzdata" // Europe/Paris must exist whatever the base image ships
)

const usage = `usage: sos-vpdive <command>

commands:
  serve            run the web server for both domains
  hash-password    read a password and print its argon2id hash for admins.yaml
  backup <file>    write a consistent copy of the database to <file>
  restore <file>   replace the database with <file> (stop the service first)
  healthcheck      exit 0 when the local server answers /healthz
  validate-kb      check the fiches of kb/ and list the marks left to fill in
`

// usageError reports a wrong command line.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func main() {
	err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdin, os.Stdout)
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	if _, ok := errors.AsType[usageError](err); ok {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	os.Exit(1)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError{"missing command"}
	}
	switch args[0] {
	case "serve":
		return serve(ctx, getenv, stdout)
	case "hash-password":
		return hashPassword(stdin, stdout)
	case "backup":
		if len(args) != 2 {
			return usageError{"backup needs a destination file"}
		}
		return backup(ctx, getenv, args[1], stdout)
	case "restore":
		if len(args) != 2 {
			return usageError{"restore needs a backup file"}
		}
		return restore(ctx, getenv, args[1], stdout)
	case "healthcheck":
		return healthcheck(ctx, getenv)
	case "validate-kb":
		return validateKB(stdout)
	default:
		return usageError{fmt.Sprintf("unknown command %q", args[0])}
	}
}
