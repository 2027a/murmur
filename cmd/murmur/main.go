package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"git.2027a.net/mathieu/murmur/internal/cli"
)

func run(args []string, stdout, stderr io.Writer) int {
	return cli.Run(context.Background(), args, stdout, stderr)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
