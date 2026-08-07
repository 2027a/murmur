package main

import (
	"context"
	"errors"
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

func newSignalStdin(file *os.File) (*signalStdin, error) {
	if err := syscall.SetNonblock(int(file.Fd()), true); err != nil {
		return nil, err
	}
	return &signalStdin{file: file}, nil
}

func (s *signalStdin) Read(buffer []byte) (int, error) {
	for {
		if s.cancelled.Load() {
			return 0, io.EOF
		}
		n, err := syscall.Read(int(s.file.Fd()), buffer)
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return n, err
	}
}

func (s *signalStdin) CancelRead() error {
	s.cancelled.Store(true)
	return nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "dictate" {
		stdin, err := newSignalStdin(os.Stdin)
		if err != nil {
			os.Exit(1)
		}
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
