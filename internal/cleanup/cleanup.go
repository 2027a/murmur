package cleanup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/events"
	"git.2027a.net/2027a/murmur/internal/policy"
)

const (
	DefaultPrompt    = "Corrige uniquement l'orthographe, la ponctuation et la syntaxe légère du texte fourni. Ne résume pas, ne commente pas, ne réponds pas au contenu et n'en change pas le sens. Retourne uniquement le texte corrigé. Préserve exactement et dans le même ordre tous les tokens sentinelles de forme [[MURMUR_CMD:...]]."
	WarningDegraded  = "cleanup degraded: texte d'entrée conservé"
	maxResponseBytes = 4 << 20
)

var sentinelPattern = regexp.MustCompile(`(__[A-Z][A-Z0-9_]*__|\[\[MURMUR_CMD:[^]\r\n]*\]\])`)

type Config struct {
	BaseURL         string
	Model           string
	Prompt          string
	APIKey          string
	Timeout         time.Duration
	AllowedZone     policy.Zone
	AllowInsecure   bool
	TrustedNetworks []netip.Prefix
}

type Result struct {
	Text     string
	Warnings []string
	Degraded error
}

type Client struct {
	endpoint string
	model    string
	prompt   string
	apiKey   string
	timeout  time.Duration
	client   *http.Client
}

func NewOpenAICompatible(config Config) (*Client, error) {
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errs.New(errs.ConfigInvalid, "cleanup", "base_url doit être une URL HTTP(S) absolue")
	}
	if config.AllowedZone == policy.Internet && parsed.Scheme != "https" && !config.AllowInsecure {
		return nil, errs.New(errs.ConfigInvalid, "cleanup", "base_url doit utiliser HTTPS en zone internet, sauf allow_insecure=true")
	}
	if config.Timeout <= 0 {
		return nil, errs.New(errs.ConfigInvalid, "cleanup", "timeout doit être strictement positif")
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, errs.New(errs.ConfigInvalid, "cleanup", "modèle manquant")
	}
	prompt := strings.TrimSpace(config.Prompt)
	if prompt == "" {
		prompt = DefaultPrompt
	}
	transport := policy.TransportWithTrusted(config.AllowedZone, config.TrustedNetworks)
	endpoint, err := url.JoinPath(parsed.String(), "chat/completions")
	if err != nil {
		return nil, errs.Wrap(errs.ConfigInvalid, "cleanup", "construction de l'endpoint impossible", err)
	}
	return &Client{
		endpoint: endpoint,
		model:    config.Model,
		prompt:   prompt,
		apiKey:   strings.TrimSpace(config.APIKey),
		timeout:  config.Timeout,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Clean always returns usable text. Degraded and Warnings explain why the
// original input was retained when the provider or a guard failed.
func (c *Client) Clean(ctx context.Context, input string) Result {
	if strings.TrimSpace(input) == "" {
		return Result{Text: input}
	}
	defer events.Measure(ctx, "cleanup")()
	output, err := c.request(ctx, input)
	if err == nil {
		err = validateOutput(input, output)
	}
	if err != nil {
		return Result{Text: input, Warnings: []string{WarningDegraded}, Degraded: err}
	}
	return Result{Text: output}
}

func (c *Client) request(ctx context.Context, input string) (string, error) {
	payload, err := json.Marshal(struct {
		Model    string    `json:"model"`
		Messages []message `json:"messages"`
	}{
		Model: c.model,
		Messages: []message{
			{Role: "system", Content: c.prompt},
			{Role: "user", Content: input},
		},
	})
	if err != nil {
		return "", errs.Wrap(errs.Internal, "cleanup", "encodage de la requête impossible", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return "", errs.Wrap(errs.Internal, "cleanup", "construction de la requête impossible", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return "", classifyTransportError(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		classified := classifyStatus(response.StatusCode)
		drainAndClose(response.Body)
		return "", classified
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return "", errs.Wrap(errs.ServerError, "cleanup", "lecture de la réponse impossible", err)
	}
	if len(data) > maxResponseBytes {
		return "", errs.New(errs.ServerError, "cleanup", "réponse trop volumineuse")
	}
	var decoded struct {
		Choices []struct {
			Message message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return "", errs.Wrap(errs.ServerError, "cleanup", "réponse JSON invalide", err)
	}
	if len(decoded.Choices) == 0 {
		return "", errs.New(errs.EmptyResponse, "cleanup", "réponse sans choix")
	}
	return strings.TrimSpace(decoded.Choices[0].Message.Content), nil
}

func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxResponseBytes))
	_ = body.Close()
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func validateOutput(input, output string) error {
	if strings.TrimSpace(output) == "" {
		return errs.New(errs.EmptyResponse, "cleanup", "sortie vide rejetée")
	}
	lower := strings.ToLower(strings.TrimSpace(output))
	lower = strings.TrimLeft(lower, "\"'«>*#- \t")
	prefixes := []string{
		"voici", "voilà", "voila", "bien sûr", "bien sur", "texte corrigé", "correction:", "résultat:", "resultat:",
		"réponse:", "reponse:", "j'ai corrigé", "je ne peux", "je ne suis pas en mesure", "je suis désolé", "désolé", "desole", "en tant qu", "remarque:", "note:",
		"commentaire:", "explication:", "here is", "here's", "sure", "corrected text", "output:",
		"response:", "as an ai", "i can't", "i cannot", "i am unable", "i'm sorry", "i’m sorry", "sorry", "note:", "comment:", "explanation:", "```",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) {
			return errs.New(errs.EmptyResponse, "cleanup", "sortie méta, commentaire ou refus rejeté")
		}
	}
	want := sentinelPattern.FindAllString(input, -1)
	got := sentinelPattern.FindAllString(output, -1)
	if len(want) != len(got) {
		return errs.New(errs.EmptyResponse, "cleanup", "tokens sentinelles manquants ou ajoutés")
	}
	for i := range want {
		if want[i] != got[i] {
			return errs.New(errs.EmptyResponse, "cleanup", fmt.Sprintf("token sentinelle altéré ou déplacé à la position %d", i))
		}
	}
	return nil
}

func classifyStatus(status int) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return errs.New(errs.AuthFailed, "cleanup", fmt.Sprintf("authentification refusée (HTTP %d)", status))
	case status == http.StatusRequestTimeout:
		return errs.New(errs.Timeout, "cleanup", fmt.Sprintf("requête expirée (HTTP %d)", status))
	case status == http.StatusTooManyRequests:
		return errs.New(errs.Unavailable, "cleanup", fmt.Sprintf("provider temporairement indisponible (HTTP %d)", status))
	default:
		return errs.New(errs.ServerError, "cleanup", fmt.Sprintf("réponse HTTP inattendue: %d", status))
	}
}

func classifyTransportError(err error) error {
	class := errs.Classify(err)
	if class == errs.PolicyBlocked || class == errs.Cancelled || class == errs.Timeout {
		return errs.Wrap(class, "cleanup", "appel interrompu", err)
	}
	return errs.Wrap(errs.Unavailable, "cleanup", "provider inaccessible", err)
}
