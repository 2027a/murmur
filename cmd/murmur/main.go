package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"git.2027a.net/mathieu/murmur/internal/cli"
)

func run(args []string, stdout, stderr io.Writer) int {
	return cli.Run(context.Background(), args, stdout, stderr)
}

type signalStdin struct {
	file      *os.File
	cancelled atomic.Bool
}

func (s *signalStdin) Read(buffer []byte) (int, error) {
	n, err := s.file.Read(buffer)
	if s.cancelled.Load() && err != nil {
		return n, io.EOF
	}
	return n, err
}

func (s *signalStdin) CancelRead() error {
	s.cancelled.Store(true)
	if err := s.file.SetReadDeadline(time.Now()); err == nil {
		return nil
	}
	return s.file.Close()
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "dictate" {
		stdin := &signalStdin{file: os.Stdin}
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer signal.Stop(signals)
		go func() {
			<-signals
			_ = stdin.CancelRead()
		}()
		os.Exit((cli.App{Stdin: stdin}).Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
