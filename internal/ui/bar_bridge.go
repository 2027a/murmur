package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const barTimeout = 600 * time.Millisecond

func BarSocket(runtimeDir string) string { return filepath.Join(runtimeDir, "ui.sock") }

func CheckBar(ctx context.Context, runtimeDir string) error {
	return sendBarState(ctx, BarSocket(runtimeDir), BarState{Version: 1, Probe: true})
}

func sendBarState(ctx context.Context, path string, state BarState) error {
	ctx, cancel := context.WithTimeout(ctx, barTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if err := json.NewEncoder(conn).Encode(state); err != nil {
		return err
	}
	var ack [1]byte
	if _, err := io.ReadFull(conn, ack[:]); err != nil {
		return err
	}
	if ack[0] != 1 {
		return fmt.Errorf("module QuickShell sans accusé de réception")
	}
	return nil
}

// ServeBar is owned by the shell's Process (stdin/stdout). Every snapshot must
// be acknowledged by QML with its sequence number on stdin. A listening socket
// alone, a stopped shell, or a shell without the widget is not availability.
// One small exchange per connection also lets doctor probe a running session.
func ServeBar(ctx context.Context, runtimeDir string, stdin io.Reader, stdout io.Writer) error {
	lock, err := os.OpenFile(filepath.Join(runtimeDir, "ui.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("un module Murmur est déjà connecté: %w", err)
	}
	path := BarSocket(runtimeDir)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(path, 0600); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-ctx.Done(); _ = listener.Close() }()
	acks := make(chan uint64, 8)
	go func() {
		defer cancel() // EOF/HUP means the owning shell is gone.
		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 64), 128)
		for scanner.Scan() {
			seq, err := strconv.ParseUint(scanner.Text(), 10, 64)
			if err != nil {
				continue
			}
			select {
			case acks <- seq:
			case <-ctx.Done():
				return
			}
		}
	}()
	var sequence uint64
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		sequence++
		err = bridgeExchange(ctx, conn, stdout, acks, sequence)
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func bridgeExchange(ctx context.Context, conn net.Conn, stdout io.Writer, acks <-chan uint64, sequence uint64) error {
	_ = conn.SetDeadline(time.Now().Add(barTimeout))
	var state BarState
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&state); err != nil || state.Version != 1 {
		return nil
	}
	state.Sequence = sequence
	if err := writeBarFrame(ctx, stdout, state); err != nil {
		return err
	}
	timer := time.NewTimer(barTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case ack := <-acks:
			if ack == sequence {
				_, _ = conn.Write([]byte{1})
				return nil
			}
		}
	}
}

// An inherited stdout pipe need not support SetWriteDeadline. If its reader
// stops draining it, terminate this dedicated bridge process instead of keeping
// ui.lock forever across a shell reload. At most one write can be outstanding;
// on timeout ServeBar returns and the CLI process exits, including this worker.
func writeBarFrame(ctx context.Context, stdout io.Writer, state BarState) error {
	done := make(chan error, 1)
	go func() { done <- json.NewEncoder(stdout).Encode(state) }()
	timer := time.NewTimer(barTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("flux QuickShell bloqué")
	}
}
