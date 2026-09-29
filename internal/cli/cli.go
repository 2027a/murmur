package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"git.2027a.net/2027a/murmur/internal/config"
	"git.2027a.net/2027a/murmur/internal/doctor"
	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/ipc"
	"git.2027a.net/2027a/murmur/internal/session"
	"git.2027a.net/2027a/murmur/internal/ui"
	"git.2027a.net/2027a/murmur/internal/xdg"
)

const usage = `Usage: murmur <commande> [options]

Commandes:
  start [--clipboard-only]  démarrer une session
  stop                      arrêter et traiter la capture
  cancel                    annuler avant la livraison
  toggle [--clipboard-only] démarrer ou arrêter l'écoute
  status [--json]           afficher l'état courant
  doctor [--json]           vérifier la configuration effective
  ui                       flux JSONL pour le module QuickShell (accusés sur stdin)
  transcribe [--json] [--events] <fichier.wav>
                            transcrire un WAV sans capture desktop
  cleanup [--json] [--events]
                             nettoyer stdin vers stdout sans capture
  dictate [--sink stdout|file] [--json] [--events]
                             capturer au premier plan; stdout par défaut
`

type App struct {
	ConfigPath string
	Stdin      io.Reader
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return (App{}).Run(ctx, args, stdout, stderr)
}

func (app App) Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		_, _ = io.WriteString(stdout, usage)
		return 0
	}
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return 2
	}

	var err error
	switch args[0] {
	case "start":
		clipboardOnly, ok := boolFlag("start", args[1:], "clipboard-only")
		if !ok {
			return usageError(stderr, "usage invalide pour start")
		}
		err = app.start(ctx, stdout, clipboardOnly)
	case "stop":
		if !noFlags("stop", args[1:]) {
			return usageError(stderr, "usage invalide pour stop")
		}
		err = app.command(ctx, ipc.Stop)
	case "cancel":
		if !noFlags("cancel", args[1:]) {
			return usageError(stderr, "usage invalide pour cancel")
		}
		err = app.command(ctx, ipc.Cancel)
	case "toggle":
		clipboardOnly, ok := boolFlag("toggle", args[1:], "clipboard-only")
		if !ok {
			return usageError(stderr, "usage invalide pour toggle")
		}
		err = app.toggle(ctx, stdout, clipboardOnly)
	case "status":
		jsonOutput, ok := boolFlag("status", args[1:], "json")
		if !ok {
			return usageError(stderr, "usage invalide pour status")
		}
		err = app.status(ctx, stdout, jsonOutput)
	case "doctor":
		jsonOutput, ok := boolFlag("doctor", args[1:], "json")
		if !ok {
			return usageError(stderr, "usage invalide pour doctor")
		}
		var healthy bool
		healthy, err = app.doctor(ctx, stdout, jsonOutput)
		if err == nil && !healthy {
			return 1
		}
	case "transcribe":
		return app.transcribeCommand(ctx, args[1:], stdout, stderr)
	case "cleanup":
		return app.cleanupCommand(ctx, args[1:], stdout, stderr)
	case "dictate":
		return app.dictateCommand(ctx, args[1:], stdout, stderr)
	case "ui":
		if !noFlags("ui", args[1:]) {
			return usageError(stderr, "usage invalide pour ui")
		}
		var runtimeDir string
		runtimeDir, err = app.runtime()
		if err == nil {
			err = ui.ServeBar(ctx, runtimeDir, app.stdin(), stdout)
		}
	case "__session":
		err = app.runSession(ctx, args[1:], stdout)
	default:
		return usageError(stderr, "commande inconnue: "+args[0])
	}
	if err != nil {
		if args[0] != "__session" {
			fmt.Fprintln(stderr, err)
		}
		return 1
	}
	return 0
}

func (app App) stdin() io.Reader {
	if app.Stdin != nil {
		return app.Stdin
	}
	return os.Stdin
}

func boolFlag(command string, args []string, name string) (bool, bool) {
	prefix := "--" + name
	seen := 0
	for _, arg := range args {
		if arg == prefix || strings.HasPrefix(arg, prefix+"=") {
			seen++
		}
	}
	if seen > 1 {
		return false, false
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	value := flags.Bool(name, false, "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return false, false
	}
	return *value, true
}

func noFlags(command string, args []string) bool {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags.Parse(args) == nil && flags.NArg() == 0
}

func usageError(stderr io.Writer, message string) int {
	fmt.Fprintf(stderr, "%s\n\n%s", message, usage)
	return 2
}

func (app App) resolve(overrides config.Overrides) (config.Config, error) {
	return config.Resolve(app.ConfigPath, overrides)
}

func (app App) runtime() (string, error) {
	return xdg.RuntimeDir()
}

func socketPath(runtimeDir string) string {
	return filepath.Join(runtimeDir, "session.sock")
}

func (app App) command(ctx context.Context, command ipc.Command) error {
	runtimeDir, err := app.runtime()
	if err != nil {
		return err
	}
	_, err = ipc.NewClient(socketPath(runtimeDir)).Do(ctx, command)
	return err
}

func (app App) toggle(ctx context.Context, stdout io.Writer, clipboardOnly bool) error {
	err := app.command(ctx, ipc.Toggle)
	if errors.Is(err, ipc.ErrNoSession) {
		return app.start(ctx, stdout, clipboardOnly)
	}
	return err
}

type statusOutput struct {
	Version         int                 `json:"v"`
	Active          bool                `json:"active"`
	State           string              `json:"state"`
	LastResult      *session.LastResult `json:"last_result,omitempty"`
	LastResultStale bool                `json:"last_result_stale,omitempty"`
}

func (app App) status(ctx context.Context, stdout io.Writer, jsonOutput bool) error {
	runtimeDir, err := app.runtime()
	if err != nil {
		return err
	}
	response, err := ipc.NewClient(socketPath(runtimeDir)).Do(ctx, ipc.Status)
	status := statusOutput{Version: 1, Active: true, State: response.State}
	if errors.Is(err, ipc.ErrNoSession) {
		status.Active = false
		status.State = string(session.Idle)
		last, readErr := session.ReadLastResult(runtimeDir)
		if readErr == nil {
			status.LastResult = &last
		} else if errors.Is(readErr, session.ErrLastResultStale) {
			status.LastResultStale = true
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
	} else if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(stdout).Encode(status)
	}
	if status.Active {
		_, err = fmt.Fprintln(stdout, status.State)
	} else if status.LastResultStale {
		_, err = fmt.Fprintln(stdout, "idle (dernier résultat périmé: persistance incomplète)")
	} else if status.LastResult != nil {
		if hasJournalWarning(status.LastResult.Warnings) {
			_, err = fmt.Fprintf(stdout, "idle (dernier résultat: %s, journal dégradé)\n", status.LastResult.Result)
		} else {
			_, err = fmt.Fprintf(stdout, "idle (dernier résultat: %s)\n", status.LastResult.Result)
		}
	} else {
		_, err = fmt.Fprintln(stdout, "idle")
	}
	return err
}

func hasJournalWarning(warnings []session.ResultWarning) bool {
	for _, warning := range warnings {
		if warning.Component == "session-journal" {
			return true
		}
	}
	return false
}

func (app App) doctor(ctx context.Context, stdout io.Writer, jsonOutput bool) (bool, error) {
	cfg, err := app.resolve(config.Overrides{})
	if err != nil {
		return false, err
	}
	report := doctor.Run(ctx, cfg, doctor.Dependencies{})
	if jsonOutput {
		err = report.WriteJSON(stdout)
	} else {
		err = report.WriteHuman(stdout)
	}
	return report.OK, err
}

func internalArgs(lockFD, readyFD, configFD int) []string {
	return []string{
		"__session",
		"--lock-fd=" + strconv.Itoa(lockFD),
		"--ready-fd=" + strconv.Itoa(readyFD),
		"--config-fd=" + strconv.Itoa(configFD),
	}
}

func parseInternalArgs(args []string) (int, int, int, error) {
	lockFD, readyFD, configFD := -1, -1, -1
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--lock-fd="):
			value, err := strconv.Atoi(strings.TrimPrefix(arg, "--lock-fd="))
			if err != nil || lockFD >= 0 {
				return 0, 0, 0, fmt.Errorf("argument interne lock-fd invalide")
			}
			lockFD = value
		case strings.HasPrefix(arg, "--ready-fd="):
			value, err := strconv.Atoi(strings.TrimPrefix(arg, "--ready-fd="))
			if err != nil || readyFD >= 0 {
				return 0, 0, 0, fmt.Errorf("argument interne ready-fd invalide")
			}
			readyFD = value
		case strings.HasPrefix(arg, "--config-fd="):
			value, err := strconv.Atoi(strings.TrimPrefix(arg, "--config-fd="))
			if err != nil || configFD >= 0 {
				return 0, 0, 0, fmt.Errorf("argument interne config-fd invalide")
			}
			configFD = value
		default:
			return 0, 0, 0, fmt.Errorf("argument interne inconnu: %s", arg)
		}
	}
	if lockFD < 3 || readyFD < 3 || configFD < 3 || lockFD == readyFD || lockFD == configFD || readyFD == configFD {
		return 0, 0, 0, fmt.Errorf("descripteurs internes invalides")
	}
	return lockFD, readyFD, configFD, nil
}

func bootstrapError(err error) bootstrapMessage {
	message := bootstrapMessage{OK: false, Error: &ipc.Error{Class: errs.Classify(err), Component: "session", Message: err.Error()}}
	var typed *errs.Error
	if errors.As(err, &typed) {
		message.Error.Component = typed.Component
		message.Error.Message = typed.Msg
	}
	return message
}

type bootstrapMessage struct {
	OK    bool       `json:"ok"`
	Error *ipc.Error `json:"error,omitempty"`
}

const bootstrapTimeout = 10 * time.Second
