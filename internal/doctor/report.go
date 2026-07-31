package doctor

import (
	"encoding/json"
	"fmt"
	"io"
)

type Status string

const (
	OK      Status = "ok"
	Failed  Status = "failed"
	Skipped Status = "skipped"
)

type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type Report struct {
	Version int     `json:"v"`
	OK      bool    `json:"ok"`
	Checks  []Check `json:"checks"`
}

func (r Report) WriteHuman(w io.Writer) error {
	for _, check := range r.Checks {
		label := "OK"
		if check.Status == Failed {
			label = "ECHEC"
		} else if check.Status == Skipped {
			label = "IGNORE"
		}
		if check.Detail == "" {
			if _, err := fmt.Fprintf(w, "[%s] %s\n", label, check.Name); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "[%s] %s: %s\n", label, check.Name, check.Detail); err != nil {
			return err
		}
	}
	return nil
}

func (r Report) WriteJSON(w io.Writer) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(r)
}
