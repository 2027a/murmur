package doctor

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"git.2027a.net/2027a/murmur/internal/config"
	"git.2027a.net/2027a/murmur/internal/core"
	"git.2027a.net/2027a/murmur/internal/inject"
	"git.2027a.net/2027a/murmur/internal/sysexec"
	"git.2027a.net/2027a/murmur/internal/ui"
	"git.2027a.net/2027a/murmur/internal/xdg"
)

type SecretResolver interface {
	ResolveCredentials(context.Context, config.CredentialConfig) (string, error)
}

type ProviderChecker interface {
	Check(context.Context, string, config.ProviderConfig, string) error
}

type SourceChecker interface {
	Check(context.Context, string) error
}

type Dependencies struct {
	LookPath        func(string) (string, error)
	Runtime         func() error
	File            func(string) error
	Notifications   func(context.Context) error
	QuickShell      func(context.Context) error
	Executor        sysexec.Executor
	SecretExecutor  sysexec.Executor
	Secrets         SecretResolver
	Providers       ProviderChecker
	Source          SourceChecker
	OSDHelper       string
	ExternalTimeout time.Duration
	TrustedNetworks []netip.Prefix
}

const DefaultExternalCheckTimeout = 5 * time.Second

func Run(ctx context.Context, cfg config.Config, dependencies Dependencies) Report {
	d := withDefaults(cfg, dependencies)
	report := Report{Version: 1, OK: true, Checks: make([]Check, 0, 16)}
	add := func(name string, err error) {
		check := Check{Name: name, Status: OK}
		if err != nil {
			check.Status = Failed
			check.Detail = err.Error()
			report.OK = false
		}
		report.Checks = append(report.Checks, check)
	}

	if err := config.Validate(cfg); err != nil {
		add("configuration et policy", err)
		return report
	}
	add("configuration et policy", nil)
	add("repertoire runtime", d.Runtime())
	addTool(&report, d.LookPath, "capture: pw-record", "pw-record")
	add("source audio", timedCheck(ctx, d.ExternalTimeout, func(checkCtx context.Context) error {
		return d.Source.Check(checkCtx, cfg.Audio.Source)
	}))

	for _, provider := range requiredProviders(cfg) {
		secret := ""
		if hasSecretSource(provider.credentials) {
			var resolved string
			err := timedCheck(ctx, d.ExternalTimeout, func(checkCtx context.Context) error {
				var err error
				resolved, err = d.Secrets.ResolveCredentials(checkCtx, provider.credentials)
				return err
			})
			add("credential "+provider.label, err)
			if err != nil {
				report.Checks = append(report.Checks, Check{
					Name:   "sante " + provider.label,
					Status: Skipped,
					Detail: "credential indisponible",
				})
				continue
			}
			secret = resolved
		}
		if provider.health == nil || !provider.health.HealthEnabled {
			report.Checks = append(report.Checks, Check{
				Name: "sante " + provider.label, Status: Skipped, Detail: "health désactivé",
			})
			continue
		}
		add("sante "+provider.label, timedCheck(ctx, d.ExternalTimeout, func(checkCtx context.Context) error {
			return d.Providers.Check(checkCtx, provider.name, *provider.health, secret)
		}))
	}

	for _, tool := range sinkTools(cfg) {
		addTool(&report, d.LookPath, "sink: "+tool, tool)
	}
	if usesHyprlandPaste(cfg) {
		add("sink: session Hyprland", timedCheck(ctx, d.ExternalTimeout, func(checkCtx context.Context) error {
			return inject.CheckHyprland(checkCtx, d.Executor)
		}))
	}
	if cfg.Sink.Mode == "file" {
		add("fichier de sortie", d.File(cfg.Sink.File))
	}

	switch cfg.UI.Mode {
	case "auto", "quickshell":
		err := timedCheck(ctx, d.ExternalTimeout, d.QuickShell)
		if err != nil && cfg.UI.Mode == "auto" {
			report.Checks = append(report.Checks, Check{Name: "UI QuickShell", Status: Skipped, Detail: "module absent ou sans réponse; repli notifications"})
			add("UI notifications D-Bus", timedCheck(ctx, d.ExternalTimeout, d.Notifications))
		} else {
			add("UI QuickShell", err)
		}
	case "notify":
		add("UI notifications D-Bus", timedCheck(ctx, d.ExternalTimeout, d.Notifications))
	case "osd":
		helper := cfg.UI.OSD.Helper
		if d.OSDHelper != "" {
			helper = d.OSDHelper
		}
		if _, err := d.LookPath(helper); err != nil {
			add("UI helper OSD", fmt.Errorf("%s introuvable", helper))
		} else {
			err := timedCheck(ctx, d.ExternalTimeout, func(checkCtx context.Context) error {
				_, err := d.Executor.Run(checkCtx, helper, []string{"--check"}, nil)
				return err
			})
			add("UI helper OSD", err)
		}
	}
	if cfg.UI.Sound {
		addAnyTool(&report, d.LookPath, "UI son", []string{"pw-play", "paplay", "canberra-gtk-play"})
	}
	return report
}

func timedCheck(ctx context.Context, timeout time.Duration, check func(context.Context) error) error {
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := check(checkCtx)
	if checkCtx.Err() != nil {
		return checkCtx.Err()
	}
	return err
}

type requiredProvider struct {
	label       string
	name        string
	credentials config.CredentialConfig
	health      *config.ProviderConfig
}

func requiredProviders(cfg config.Config) []requiredProvider {
	seen := make(map[string]bool)
	providers := make([]requiredProvider, 0, 3)
	addSTT := func(name string) {
		kind := "STT"
		key := kind + ":" + name
		if name == "" || seen[key] {
			return
		}
		seen[key] = true
		provider := cfg.STT.Providers[name]
		providers = append(providers, requiredProvider{
			label: kind + " " + name, name: name, credentials: provider.Credentials(), health: &provider,
		})
	}
	addSTT(cfg.STT.Provider)
	addSTT(cfg.STT.Fallback)
	if cfg.Cleanup.Enabled {
		name := cfg.Cleanup.Provider
		provider := cfg.Cleanup.Providers[name]
		providers = append(providers, requiredProvider{
			label: "cleanup " + name, name: name, credentials: provider.Credentials(),
		})
	}
	return providers
}

func sinkTools(cfg config.Config) []string {
	needed := make(map[string]bool)
	if cfg.Sink.ClipboardMode != "none" {
		needed["wl-copy"] = true
	}
	mode := cfg.Sink.Mode
	if mode == "auto" {
		mode = cfg.Sink.AutoMode
	}
	switch mode {
	case "paste":
		if usesHyprlandPaste(cfg) {
			needed["hyprctl"] = true
		} else {
			needed["wtype"] = true
		}
	case "type":
		needed["wtype"] = true
	case "clipboard":
		needed["wl-copy"] = true
	}
	tools := make([]string, 0, len(needed))
	for tool := range needed {
		tools = append(tools, tool)
	}
	sort.Strings(tools)
	return tools
}

func usesHyprlandPaste(cfg config.Config) bool {
	paste := cfg.Sink.Mode == "paste" || (cfg.Sink.Mode == "auto" && cfg.Sink.AutoMode == "paste")
	return paste && cfg.Sink.PasteBackend == "hyprland" && cfg.Sink.ClipboardMode != "none" && len(cfg.Sink.PasteShortcuts) > 0
}

func addTool(report *Report, lookup func(string) (string, error), name, tool string) {
	_, err := lookup(tool)
	if err != nil {
		report.OK = false
		report.Checks = append(report.Checks, Check{Name: name, Status: Failed, Detail: tool + " introuvable"})
		return
	}
	report.Checks = append(report.Checks, Check{Name: name, Status: OK})
}

func addAnyTool(report *Report, lookup func(string) (string, error), name string, tools []string) {
	for _, tool := range tools {
		if _, err := lookup(tool); err == nil {
			report.Checks = append(report.Checks, Check{Name: name, Status: OK, Detail: tool})
			return
		}
	}
	report.OK = false
	report.Checks = append(report.Checks, Check{
		Name:   name,
		Status: Failed,
		Detail: "aucun lecteur disponible (" + strings.Join(tools, ", ") + ")",
	})
}

func hasSecretSource(provider config.CredentialConfig) bool {
	return provider.APIKeyEnv != "" || provider.APIKeyFile != "" || len(provider.APIKeyCommand) > 0
}

func withDefaults(cfg config.Config, d Dependencies) Dependencies {
	executorInjected := d.Executor != nil
	environment := os.Environ()
	if d.LookPath == nil {
		d.LookPath = exec.LookPath
	}
	if d.Runtime == nil {
		d.Runtime = checkRuntime
	}
	if d.File == nil {
		d.File = checkOutputFile
	}
	if d.Notifications == nil {
		d.Notifications = ui.CheckDesktopNotifications
	}
	if d.QuickShell == nil {
		d.QuickShell = func(ctx context.Context) error {
			runtimeDir, err := xdg.RuntimeDir()
			if err != nil {
				return err
			}
			return ui.CheckBar(ctx, runtimeDir)
		}
	}
	if d.ExternalTimeout <= 0 {
		d.ExternalTimeout = DefaultExternalCheckTimeout
	}
	if d.Executor == nil {
		d.Executor = sysexec.OSExecutor{Env: config.CredentialHelperEnvironment(cfg, environment)}
	}
	if d.Secrets == nil {
		secretExecutor := d.SecretExecutor
		if secretExecutor == nil {
			if executorInjected {
				secretExecutor = d.Executor
			} else {
				secretExecutor = sysexec.OSExecutor{Env: config.CredentialHelperEnvironment(cfg, environment)}
			}
		}
		d.Secrets = config.SecretResolver{Runner: secretRunner{executor: secretExecutor}}
	}
	if d.Source == nil {
		d.Source = pipeWireSourceChecker{executor: d.Executor}
	}
	if len(d.TrustedNetworks) == 0 {
		for _, value := range cfg.Policy.TrustedNetworks {
			if prefix, err := netip.ParsePrefix(value); err == nil {
				d.TrustedNetworks = append(d.TrustedNetworks, prefix)
			}
		}
	}
	if d.Providers == nil {
		d.Providers = httpProviderChecker{trusted: d.TrustedNetworks}
	}
	return d
}

type secretRunner struct{ executor sysexec.Executor }

func (r secretRunner) Run(ctx context.Context, name string, args []string) ([]byte, error) {
	return sysexec.RunLimited(ctx, r.executor, name, args, nil, config.MaxSecretBytes)
}

type pipeWireSourceChecker struct{ executor sysexec.Executor }

func (c pipeWireSourceChecker) Check(ctx context.Context, source string) error {
	if strings.TrimSpace(source) == "" {
		return nil
	}
	_, err := c.executor.Run(ctx, "pw-cli", []string{"info", source}, nil)
	if err != nil {
		return fmt.Errorf("source %q introuvable", source)
	}
	return nil
}

type httpProviderChecker struct {
	trusted []netip.Prefix
}

func (c httpProviderChecker) Check(ctx context.Context, _ string, provider config.ProviderConfig, secret string) error {
	checker, err := core.NewSTTProvider("doctor", provider, secret, c.trusted)
	if err != nil {
		return err
	}
	return checker.Health(ctx)
}

func checkRuntime() error {
	dir, err := xdg.RuntimeDir()
	if err != nil {
		return err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("permissions attendues: 0700")
	}
	file, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return fmt.Errorf("repertoire non accessible en ecriture")
	}
	name := file.Name()
	if closeErr := file.Close(); closeErr != nil {
		os.Remove(name)
		return closeErr
	}
	return os.Remove(name)
}

func checkOutputFile(path string) error {
	path = filepath.Clean(path)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("le chemin existe mais n'est pas un fichier regulier")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(path)
	if info, err := os.Lstat(parent); err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("repertoire de sortie invalide")
	}
	file, err := os.CreateTemp(parent, ".murmur-doctor-*")
	if err != nil {
		return fmt.Errorf("repertoire de sortie non accessible en ecriture")
	}
	source := file.Name()
	target := source + ".replace"
	defer os.Remove(source)
	defer os.Remove(target)
	if _, err := file.Write([]byte("probe\n")); err != nil {
		_ = file.Close()
		return fmt.Errorf("fichier de sonde non accessible en ecriture")
	}
	if closeErr := file.Close(); closeErr != nil {
		return closeErr
	}
	if err := os.Rename(source, target); err != nil {
		return fmt.Errorf("remplacement atomique indisponible")
	}
	return os.Remove(target)
}
