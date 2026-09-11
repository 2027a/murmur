//go:build unix

package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"git.2027a.net/mathieu/murmur/internal/config"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
	"git.2027a.net/mathieu/murmur/internal/ipc"
	"git.2027a.net/mathieu/murmur/internal/logging"
	"git.2027a.net/mathieu/murmur/internal/session"
)

func (app App) start(ctx context.Context, stdout io.Writer, clipboardOnly bool) error {
	cfg, err := app.resolve(config.Overrides{})
	if err != nil {
		return err
	}
	if err := config.ValidateClipboardOnly(cfg, clipboardOnly); err != nil {
		return err
	}
	runtimeDir, err := app.runtime()
	if err != nil {
		return err
	}
	lock, err := session.AcquireLock(runtimeDir)
	if err != nil {
		return err
	}
	owned := true
	defer func() {
		if owned {
			_ = lock.Close()
		}
	}()

	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	defer readyReader.Close()
	configReader, configWriter, err := os.Pipe()
	if err != nil {
		readyWriter.Close()
		return err
	}
	defer configWriter.Close()
	executable, err := os.Executable()
	if err != nil {
		readyWriter.Close()
		configReader.Close()
		return err
	}
	logDestination, err := logging.OpenDestination(logging.Path(runtimeDir))
	if err != nil {
		readyWriter.Close()
		configReader.Close()
		return fmt.Errorf("ouverture du log opérationnel: %w", err)
	}
	defer logDestination.Close()
	cmd := exec.Command(executable, internalArgs(3, 4, 5)...)
	cmd.ExtraFiles = []*os.File{lock.File(), readyWriter, configReader}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		readyWriter.Close()
		configReader.Close()
		return err
	}
	defer devNull.Close()
	cmd.Stdin = devNull
	cmd.Stderr = devNull
	cmd.Stdout = devNull
	if cfg.Sink.Mode == "stdout" && !clipboardOnly {
		cmd.Stdout = stdout
	}
	if err := cmd.Start(); err != nil {
		readyWriter.Close()
		configReader.Close()
		return err
	}
	readyWriter.Close()
	configReader.Close()
	if err := json.NewEncoder(configWriter).Encode(sessionSnapshot{Config: cfg, ClipboardOnly: clipboardOnly}); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("transmission de la configuration effective: %w", err)
	}
	configWriter.Close()
	if err := lock.Detach(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	owned = false
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	messageDone := make(chan struct {
		message bootstrapMessage
		err     error
	}, 1)
	go func() {
		var message bootstrapMessage
		err := json.NewDecoder(readyReader).Decode(&message)
		messageDone <- struct {
			message bootstrapMessage
			err     error
		}{message, err}
	}()
	timer := time.NewTimer(bootstrapTimeout)
	defer timer.Stop()
	select {
	case result := <-messageDone:
		if result.err != nil {
			_ = cmd.Process.Kill()
			childErr := <-waitDone
			return fmt.Errorf("réponse de bootstrap invalide: %v: %w", childErr, result.err)
		}
		if !result.message.OK {
			_ = cmd.Process.Kill()
			<-waitDone
			if result.message.Error == nil {
				return fmt.Errorf("bootstrap de session refusé")
			}
			return errs.New(result.message.Error.Class, result.message.Error.Component, result.message.Error.Message)
		}
		return nil
	case childErr := <-waitDone:
		return fmt.Errorf("processus de session arrêté pendant le bootstrap: %v", childErr)
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-waitDone
		return fmt.Errorf("délai de bootstrap de session dépassé")
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-waitDone
		return ctx.Err()
	}
}

func (app App) runSession(ctx context.Context, args []string, stdout io.Writer) (returnErr error) {
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stopSignals()
	lockFD, readyFD, configFD, err := parseInternalArgs(args)
	if err != nil {
		return err
	}
	if err := validateSessionFDs(lockFD, readyFD, configFD); err != nil {
		return err
	}
	syscall.CloseOnExec(readyFD)
	syscall.CloseOnExec(configFD)
	ready := os.NewFile(uintptr(readyFD), "murmur-bootstrap")
	if ready == nil {
		return fmt.Errorf("descripteur de bootstrap absent")
	}
	bootstrapSent := false
	defer func() {
		if !bootstrapSent && returnErr != nil {
			_ = json.NewEncoder(ready).Encode(bootstrapError(returnErr))
		}
		_ = ready.Close()
	}()
	runtimeDir, err := app.runtime()
	if err != nil {
		return err
	}
	lockFile := os.NewFile(uintptr(lockFD), "murmur-session.lock")
	lock, err := session.AdoptLock(lockFile, filepath.Join(runtimeDir, "session.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()

	configFile := os.NewFile(uintptr(configFD), "murmur-config.json")
	if configFile == nil {
		return fmt.Errorf("descripteur de configuration absent")
	}
	var snapshot sessionSnapshot
	err = decodeSessionSnapshot(configFile, &snapshot)
	_ = configFile.Close()
	if err != nil {
		return err
	}
	cfg := snapshot.Config
	if err := config.Validate(cfg); err != nil {
		return err
	}
	logging.Configure(cfg.Logging.Level)
	if err := logging.ConfigureFile(cfg.Logging.Level, logging.Path(runtimeDir), int64(cfg.Logging.MaxBytes), cfg.Logging.MaxFiles); err != nil {
		logging.Warnf(logging.ComponentLogging, "destination rotative indisponible")
	} else {
		defer func() { _ = logging.Close() }()
	}
	logging.Debugf(logging.ComponentSession, "configuration de session validée")
	defer func() {
		if returnErr != nil {
			logging.Errorf(logging.ComponentSession, "session terminée avec une erreur")
		} else {
			logging.Infof(logging.ComponentSession, "session terminée")
		}
	}()
	if err := config.ValidateClipboardOnly(cfg, snapshot.ClipboardOnly); err != nil {
		return err
	}
	path := socketPath(runtimeDir)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("suppression du socket obsolète: %w", err)
	}
	defer os.Remove(path)
	bus := events.NewBus()
	defer bus.Close()
	var journal *events.Journal
	defer func() {
		if journal != nil {
			returnErr = errors.Join(returnErr, journal.Close())
		}
	}()
	if cfg.Logging.SessionJournal {
		journal, err = events.NewJournal(bus, filepath.Join(runtimeDir, "session.jsonl"), events.JournalOptions{
			MaxBytes: int64(cfg.Logging.SessionMaxBytes), MaxFiles: cfg.Logging.SessionMaxFiles,
			OnFailure: func(operation events.JournalOperation) {
				logging.Warnf(logging.ComponentSessionJournal, "%s", events.JournalFailureMessage(operation))
			},
		})
		if err != nil {
			return err
		}
	}
	sessionID := newSessionID()
	if err := session.MarkLastResultPending(runtimeDir, sessionID); err != nil {
		return err
	}
	bootstrap, err := session.NewBootstrap(session.BootstrapOptions{
		Context: ctx, SessionID: sessionID, Bus: bus,
	})
	if err != nil {
		return err
	}
	orchestrator, err := session.Build(session.BuildOptions{
		Context: bootstrap.Context(), Config: cfg, RuntimeDir: runtimeDir, SessionID: sessionID,
		ClipboardOnly: snapshot.ClipboardOnly, Stdout: stdout, Bus: bus, Bootstrap: bootstrap,
	})
	if err != nil {
		result := bootstrap.Finish(err, "")
		journalErr := closeSessionJournal(&journal)
		if journalErr != nil {
			session.AddJournalWarning(&result)
		}
		persistErr := session.WriteLastResult(runtimeDir, result)
		var commitErr error
		if persistErr == nil {
			commitErr = session.CommitLastResult(runtimeDir, result.SessionID)
		}
		if result.Result == session.Cancelled {
			return errors.Join(journalErr, persistErr, commitErr)
		}
		return errors.Join(err, journalErr, persistErr, commitErr)
	}
	bootstrap.Attach(orchestrator)
	server, err := ipc.Listen(path, bootstrap, bus)
	if err != nil {
		return err
	}
	serveCtx, stopServer := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(serveCtx) }()
	if err := json.NewEncoder(ready).Encode(bootstrapMessage{OK: true}); err != nil {
		stopServer()
		<-serveDone
		return err
	}
	bootstrapSent = true
	_ = ready.Close()
	logging.Infof(logging.ComponentSession, "session démarrée")

	type runOutcome struct {
		result     session.LastResult
		runErr     error
		persistErr error
	}
	runDone := make(chan runOutcome, 1)
	go func() {
		result, runErr, persistErr := orchestrator.Run()
		runDone <- runOutcome{result: result, runErr: runErr, persistErr: persistErr}
	}()
	select {
	case outcome := <-runDone:
		stopServer()
		serveErr := <-serveDone
		journalErr := closeSessionJournal(&journal)
		rewriteErr := persistJournalWarning(runtimeDir, &outcome.result, journalErr)
		persistErr := errors.Join(outcome.persistErr, rewriteErr)
		commitErr := commitLastResult(runtimeDir, outcome.result.SessionID, persistErr)
		return errors.Join(outcome.runErr, serveErr, journalErr, persistErr, commitErr)
	case serveErr := <-serveDone:
		if serveErr == nil {
			serveErr = fmt.Errorf("serveur IPC arrêté avant la session")
		}
		_ = orchestrator.Handle(context.Background(), ipc.NewRequest(ipc.Cancel))
		outcome := <-runDone
		stopServer()
		journalErr := closeSessionJournal(&journal)
		rewriteErr := persistJournalWarning(runtimeDir, &outcome.result, journalErr)
		persistErr := errors.Join(outcome.persistErr, rewriteErr)
		commitErr := commitLastResult(runtimeDir, outcome.result.SessionID, persistErr)
		return errors.Join(serveErr, outcome.runErr, journalErr, persistErr, commitErr)
	}
}

func closeSessionJournal(journal **events.Journal) error {
	if journal == nil || *journal == nil {
		return nil
	}
	err := (*journal).Close()
	*journal = nil
	return err
}

func persistJournalWarning(runtimeDir string, result *session.LastResult, journalErr error) error {
	if journalErr == nil {
		return nil
	}
	session.AddJournalWarning(result)
	return session.WriteLastResult(runtimeDir, *result)
}

func commitLastResult(runtimeDir, sessionID string, persistErr error) error {
	if persistErr != nil {
		return nil
	}
	return session.CommitLastResult(runtimeDir, sessionID)
}

type sessionSnapshot struct {
	Config        config.Config `json:"config"`
	ClipboardOnly bool          `json:"clipboard_only"`
}

const maxSessionSnapshotBytes = 1 << 20

func decodeSessionSnapshot(reader io.Reader, snapshot *sessionSnapshot) error {
	data, err := io.ReadAll(io.LimitReader(reader, maxSessionSnapshotBytes+1))
	if err != nil {
		return errs.Wrap(errs.ConfigInvalid, "session-config", "lecture de la configuration effective impossible", err)
	}
	if len(data) > maxSessionSnapshotBytes {
		return errs.New(errs.ConfigInvalid, "session-config", "configuration effective trop volumineuse")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(snapshot); err != nil {
		return errs.Wrap(errs.ConfigInvalid, "session-config", "configuration effective invalide", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errs.New(errs.ConfigInvalid, "session-config", "données supplémentaires après la configuration effective")
	}
	return nil
}

func validateSessionFDs(lockFD, readyFD, configFD int) error {
	if lockFD < 3 || readyFD < 3 || configFD < 3 || lockFD == readyFD || lockFD == configFD || readyFD == configFD {
		return errs.New(errs.Internal, "session-fd", "descripteurs internes invalides")
	}
	var stats [3]syscall.Stat_t
	for i, fd := range []int{lockFD, readyFD, configFD} {
		if err := syscall.Fstat(fd, &stats[i]); err != nil {
			return errs.Wrap(errs.Internal, "session-fd", "inspection d'un descripteur interne impossible", err)
		}
	}
	if stats[0].Mode&syscall.S_IFMT != syscall.S_IFREG {
		return errs.New(errs.Internal, "session-fd", "le descripteur de verrou n'est pas un fichier régulier")
	}
	if stats[1].Mode&syscall.S_IFMT != syscall.S_IFIFO || stats[2].Mode&syscall.S_IFMT != syscall.S_IFIFO {
		return errs.New(errs.Internal, "session-fd", "les descripteurs de bootstrap et de configuration doivent être des pipes")
	}
	for i := range stats {
		for j := i + 1; j < len(stats); j++ {
			if stats[i].Dev == stats[j].Dev && stats[i].Ino == stats[j].Ino {
				return errs.New(errs.Internal, "session-fd", "les descripteurs internes ne sont pas distincts")
			}
		}
	}
	return nil
}

func newSessionID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}
