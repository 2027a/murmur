package doctor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"git.2027a.net/2027a/murmur/internal/config"
)

func TestQuickShellSelectionAndNotificationFallback(t *testing.T) {
	for _, test := range []struct {
		mode                       string
		available, notify, healthy bool
	}{
		{"auto", true, false, true}, {"auto", false, true, true},
		{"quickshell", true, false, true}, {"quickshell", false, false, false},
		{"notify", false, true, true}, {"none", false, false, true},
	} {
		t.Run(fmt.Sprintf("%s/%v", test.mode, test.available), func(t *testing.T) {
			cfg := config.Defaults()
			cfg.UI.Mode = test.mode
			d := baseDependencies(&fakeProviders{}, &rejectingSecrets{})
			var notified, probed bool
			d.Notifications = func(context.Context) error { notified = true; return nil }
			d.QuickShell = func(context.Context) error {
				probed = true
				if !test.available {
					return errors.New("unavailable")
				}
				return nil
			}
			report := Run(context.Background(), cfg, d)
			if notified != test.notify || report.OK != test.healthy || probed != (test.mode == "auto" || test.mode == "quickshell") {
				t.Fatalf("notified=%v probed=%v report=%+v", notified, probed, report)
			}
		})
	}
}
