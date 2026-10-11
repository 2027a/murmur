package inject

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"git.2027a.net/2027a/murmur/internal/sysexec"
)

// PasteBackend selects only shortcut emission. Direct typing still uses wtype.
type PasteBackend string

const (
	Wtype    PasteBackend = "wtype"
	Hyprland PasteBackend = "hyprland"

	maxHyprlandOutput = 64 << 10
)

// CheckHyprland probes the session and dispatcher syntax without sending keys.
// The caller supplies the time budget, as for the other doctor checks.
func CheckHyprland(ctx context.Context, executor sysexec.Executor) error {
	_, err := hyprlandProvider(ctx, executor)
	return err
}

func hyprlandProvider(ctx context.Context, executor sysexec.Executor) (string, error) {
	output, err := runHyprland(ctx, executor, []string{"-j", "status"})
	if err != nil {
		return "", err
	}
	var status struct {
		Provider string `json:"configProvider"`
	}
	if json.Unmarshal(output, &status) != nil {
		return "", commandError(ctx, "état Hyprland invalide (hyprctl -j status requis)", errors.New("JSON invalide"))
	}
	switch status.Provider {
	case "lua", "hyprlang":
		return status.Provider, nil
	default:
		return "", commandError(ctx, "syntaxe du dispatcher Hyprland inconnue", errors.New("configProvider non supporté"))
	}
}

func hyprlandWindow(ctx context.Context, executor sysexec.Executor) (string, error) {
	output, err := runHyprland(ctx, executor, []string{"-j", "activewindow"})
	if err != nil {
		return "", err
	}
	var window struct {
		Address string `json:"address"`
	}
	if json.Unmarshal(output, &window) != nil || !validWindowAddress(window.Address) {
		// Do not include the response: it can contain private window titles.
		return "", commandError(ctx, "aucune fenêtre Hyprland active valide", errors.New("adresse de fenêtre invalide"))
	}
	return window.Address, nil
}

func validWindowAddress(address string) bool {
	if !strings.HasPrefix(address, "0x") || len(address) > 18 {
		return false
	}
	value, err := strconv.ParseUint(address[2:], 16, 64)
	return err == nil && value != 0
}

func runHyprland(ctx context.Context, executor sysexec.Executor, args []string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, commandError(ctx, "communication Hyprland impossible", err)
	}
	output, err := sysexec.RunLimited(ctx, executor, "hyprctl", args, nil, maxHyprlandOutput)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, commandError(ctx, "communication Hyprland impossible", err)
	}
	return output, nil
}

func (i *Injector) pasteHyprland(ctx context.Context) error {
	provider, err := hyprlandProvider(ctx, i.executor)
	if err != nil {
		return err
	}
	address, err := hyprlandWindow(ctx, i.executor)
	if err != nil {
		return err
	}
	// Do not redirect a paste to a different window if focus changed during
	// preparation. The explicit selector also prevents following later focus
	// changes between this check and dispatch. Never retry an ambiguous action.
	current, err := hyprlandWindow(ctx, i.executor)
	if err != nil {
		return err
	}
	if current != address {
		return commandError(ctx, "la fenêtre active a changé avant le collage", errors.New("collage annulé"))
	}

	mods, key := hyprlandShortcut(i.shortcut)
	var args []string
	if provider == "lua" {
		// Only fixed shortcut components and a validated hexadecimal address
		// enter Lua; provider text, titles and classes never enter executable code.
		expression := fmt.Sprintf("hl.dsp.send_shortcut({mods=%q,key=%q,window=%q})", mods, key, "address:"+address)
		args = []string{"dispatch", expression}
	} else {
		args = []string{"dispatch", "sendshortcut", mods + "," + key + ",address:" + address}
	}
	output, err := runHyprland(ctx, i.executor, args)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(output)) != "ok" {
		return commandError(ctx, "collage Hyprland non confirmé", errors.New("réponse du dispatcher inattendue"))
	}
	return nil
}

// NewWithBackend has already validated every configured shortcut.
func hyprlandShortcut(shortcut string) (mods, key string) {
	switch shortcut {
	case "ctrl+shift+v":
		return "CTRL SHIFT", "v"
	case "shift+insert":
		return "SHIFT", "Insert"
	default: // ctrl+v
		return "CTRL", "v"
	}
}
