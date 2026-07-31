package session

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/xdg"
)

type ResultError struct {
	Class     errs.Class `json:"class"`
	Component string     `json:"component,omitempty"`
	Message   string     `json:"message"`
}

type LastResult struct {
	Version    int          `json:"v"`
	SessionID  string       `json:"session_id"`
	Result     Result       `json:"result"`
	Error      *ResultError `json:"error,omitempty"`
	Provider   string       `json:"provider,omitempty"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
	Warnings   []string     `json:"warnings,omitempty"`
}

func LastResultPath(runtimeDir string) string {
	return filepath.Join(runtimeDir, "last-result.json")
}

func WriteLastResult(runtimeDir string, result LastResult) error {
	if err := result.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errs.Wrap(errs.Internal, "session-result", "encodage du résultat impossible", err)
	}
	data = append(data, '\n')
	if err := xdg.WriteFileAtomic(LastResultPath(runtimeDir), data, 0o600); err != nil {
		return errs.Wrap(errs.Internal, "session-result", "persistance du résultat impossible", err)
	}
	return nil
}

func ReadLastResult(runtimeDir string) (LastResult, error) {
	file, err := os.Open(LastResultPath(runtimeDir))
	if err != nil {
		return LastResult{}, errs.Wrap(errs.Unavailable, "session-result", "dernier résultat indisponible", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var result LastResult
	if err := decoder.Decode(&result); err != nil {
		return LastResult{}, errs.Wrap(errs.Internal, "session-result", "dernier résultat invalide", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return LastResult{}, errs.Wrap(errs.Internal, "session-result", "données supplémentaires après le résultat", err)
	}
	if err := result.Validate(); err != nil {
		return LastResult{}, err
	}
	return result, nil
}

func (r LastResult) Validate() error {
	if r.Version != 1 {
		return errs.New(errs.Internal, "session-result", fmt.Sprintf("version de résultat invalide: %d", r.Version))
	}
	if r.SessionID == "" {
		return errs.New(errs.Internal, "session-result", "identifiant de session manquant")
	}
	if r.Result != Completed && r.Result != Cancelled && r.Result != NoSpeech && r.Result != Failed {
		return errs.New(errs.Internal, "session-result", "résultat terminal invalide")
	}
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) {
		return errs.New(errs.Internal, "session-result", "horodatages du résultat invalides")
	}
	return nil
}
