package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/xdg"
)

type ResultError struct {
	Class     errs.Class `json:"class"`
	Component string     `json:"component,omitempty"`
	Message   string     `json:"message"`
}

type ResultWarning struct {
	Class     errs.Class `json:"class"`
	Provider  string     `json:"provider"`
	Component string     `json:"component"`
	Message   string     `json:"message"`
}

type LastResult struct {
	Version    int             `json:"v"`
	SessionID  string          `json:"session_id"`
	Result     Result          `json:"result"`
	Error      *ResultError    `json:"error,omitempty"`
	Provider   string          `json:"provider,omitempty"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at"`
	Warnings   []ResultWarning `json:"warnings,omitempty"`
}

type lastResultSession struct {
	Version   int    `json:"v"`
	SessionID string `json:"session_id"`
	Pending   bool   `json:"pending"`
}

var ErrLastResultStale = errors.New("dernier résultat périmé")

type atomicFileWriter func(string, []byte, os.FileMode) error

func AddJournalWarning(result *LastResult) {
	if result == nil {
		return
	}
	result.Warnings = append(result.Warnings, ResultWarning{
		Class: errs.Internal, Component: "session-journal", Message: "journal de session dégradé",
	})
}

func LastResultPath(runtimeDir string) string {
	return filepath.Join(runtimeDir, "last-result.json")
}

func LastResultSessionPath(runtimeDir string) string {
	return filepath.Join(runtimeDir, "last-result.session.json")
}

// MarkLastResultPending records the session whose terminal result is expected.
// It must complete before that session is exposed through IPC.
func MarkLastResultPending(runtimeDir, sessionID string) error {
	return markLastResultPendingWith(runtimeDir, sessionID, xdg.WriteFileAtomic)
}

func markLastResultPendingWith(runtimeDir, sessionID string, writeAtomic atomicFileWriter) error {
	if sessionID == "" {
		return errs.New(errs.Internal, "session-result", "identifiant de session manquant pour le marqueur")
	}
	marker := lastResultSession{Version: 1, SessionID: sessionID, Pending: true}
	return writeLastResultSession(runtimeDir, marker, writeAtomic)
}

// CommitLastResult makes a completely persisted session result visible to
// status. Callers commit only after runtime diagnostics have been finalized.
func CommitLastResult(runtimeDir, sessionID string) error {
	return commitLastResultWith(runtimeDir, sessionID, xdg.WriteFileAtomic)
}

func commitLastResultWith(runtimeDir, sessionID string, writeAtomic atomicFileWriter) error {
	result, err := readLastResultFile(runtimeDir)
	if err != nil {
		return err
	}
	if sessionID == "" || result.SessionID != sessionID {
		return staleLastResultError()
	}
	return writeLastResultSession(runtimeDir, lastResultSession{
		Version: 1, SessionID: sessionID,
	}, writeAtomic)
}

func writeLastResultSession(runtimeDir string, marker lastResultSession, writeAtomic atomicFileWriter) error {
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return errs.Wrap(errs.Internal, "session-result", "encodage du marqueur de résultat impossible", err)
	}
	data = append(data, '\n')
	if err := writeAtomic(LastResultSessionPath(runtimeDir), data, 0o600); err != nil {
		return errs.Wrap(errs.Internal, "session-result", "persistance du marqueur de résultat impossible", err)
	}
	return nil
}

func WriteLastResult(runtimeDir string, result LastResult) error {
	return writeLastResultWith(runtimeDir, result, xdg.WriteFileAtomic)
}

func writeLastResultWith(runtimeDir string, result LastResult, writeAtomic atomicFileWriter) error {
	if err := result.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errs.Wrap(errs.Internal, "session-result", "encodage du résultat impossible", err)
	}
	data = append(data, '\n')
	if err := writeAtomic(LastResultPath(runtimeDir), data, 0o600); err != nil {
		return errs.Wrap(errs.Internal, "session-result", "persistance du résultat impossible", err)
	}
	return nil
}

func ReadLastResult(runtimeDir string) (LastResult, error) {
	result, err := readLastResultFile(runtimeDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, markerErr := readLastResultSession(runtimeDir)
			switch {
			case markerErr == nil:
				return LastResult{}, staleLastResultError()
			case !errors.Is(markerErr, os.ErrNotExist):
				return LastResult{}, markerErr
			}
		}
		return LastResult{}, err
	}
	marker, err := readLastResultSession(runtimeDir)
	if errors.Is(err, os.ErrNotExist) {
		// Results written by versions predating the marker remain readable until
		// the next session starts.
		return result, nil
	}
	if err != nil {
		return LastResult{}, err
	}
	if marker.Pending || marker.SessionID != result.SessionID {
		return LastResult{}, staleLastResultError()
	}
	return result, nil
}

func readLastResultFile(runtimeDir string) (LastResult, error) {
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

func staleLastResultError() error {
	return errs.Wrap(
		errs.Unavailable, "session-result", "dernier résultat périmé après une session non persistée", ErrLastResultStale,
	)
}

func readLastResultSession(runtimeDir string) (lastResultSession, error) {
	file, err := os.Open(LastResultSessionPath(runtimeDir))
	if err != nil {
		return lastResultSession{}, errs.Wrap(errs.Unavailable, "session-result", "marqueur du dernier résultat indisponible", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var marker lastResultSession
	if err := decoder.Decode(&marker); err != nil {
		return lastResultSession{}, errs.Wrap(errs.Internal, "session-result", "marqueur du dernier résultat invalide", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return lastResultSession{}, errs.Wrap(errs.Internal, "session-result", "données supplémentaires après le marqueur", err)
	}
	if marker.Version != 1 || marker.SessionID == "" {
		return lastResultSession{}, errs.New(errs.Internal, "session-result", "contenu du marqueur du dernier résultat invalide")
	}
	return marker, nil
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
	switch r.Result {
	case Failed:
		if r.Error == nil {
			return errs.New(errs.Internal, "session-result", "erreur manquante pour un résultat échoué")
		}
	case Completed, Cancelled, NoSpeech:
		if r.Error != nil {
			return errs.New(errs.Internal, "session-result", "erreur présente pour un résultat non échoué")
		}
	}
	if r.Error != nil {
		if r.Error.Class == "" || r.Error.Message == "" {
			return errs.New(errs.Internal, "session-result", "erreur de résultat incomplète")
		}
	}
	for _, warning := range r.Warnings {
		if warning.Class == "" || warning.Component == "" || warning.Message == "" {
			return errs.New(errs.Internal, "session-result", "avertissement de résultat incomplet")
		}
	}
	return nil
}
