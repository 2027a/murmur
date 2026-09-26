package plugin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/policy"
)

type blockingPlugin struct{}

func (blockingPlugin) Name() string      { return "blocking" }
func (blockingPlugin) Zone() policy.Zone { return policy.Local }
func (blockingPlugin) Transform(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

type lanPlugin struct{ Plain }

func (lanPlugin) Zone() policy.Zone { return policy.LAN }

type externalPlugin struct {
	url string
}

func (externalPlugin) Name() string      { return "external" }
func (externalPlugin) Zone() policy.Zone { return policy.LAN }
func (p externalPlugin) Transform(_ context.Context, capability Capability, text string) (string, error) {
	request, err := http.NewRequest(http.MethodGet, p.url, nil)
	if err != nil {
		return "", err
	}
	response, err := capability.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	return text, nil
}

func TestPluginTimeoutIsRecoverable(t *testing.T) {
	bounded, err := New(blockingPlugin{}, Options{Timeout: 20 * time.Millisecond, AllowedZones: []policy.Zone{policy.Local}})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = bounded.Transform(context.Background(), "secret transcript")
	if errs.Classify(err) != errs.Timeout || time.Since(started) > time.Second {
		t.Fatalf("Transform() error=%v duration=%v", err, time.Since(started))
	}
	if err != nil && errors.Is(err, context.Canceled) {
		t.Fatalf("timeout classified as cancellation: %v", err)
	}
}

func TestPluginZoneMustBeAllowed(t *testing.T) {
	_, err := New(lanPlugin{}, Options{Timeout: time.Second, AllowedZones: []policy.Zone{policy.Local}})
	if errs.Classify(err) != errs.PolicyBlocked {
		t.Fatalf("New() error = %v", err)
	}
}

func TestNonLocalPluginRequiresExternalContract(t *testing.T) {
	_, err := New(lanPlugin{}, Options{Timeout: time.Second, AllowedZones: []policy.Zone{policy.LAN}})
	if errs.Classify(err) != errs.ConfigInvalid {
		t.Fatalf("New() error = %v, class = %s", err, errs.Classify(err))
	}
}

func TestExternalPluginReceivesGuardedCapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	bounded, err := New(externalPlugin{url: server.URL}, Options{
		Timeout: time.Second, AllowedZones: []policy.Zone{policy.LAN},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := bounded.Transform(context.Background(), "texte"); err != nil || got != "texte" {
		t.Fatalf("Transform() = %q, %v", got, err)
	}
}

func TestExternalCapabilityBlocksBeyondDeclaredZone(t *testing.T) {
	bounded, err := New(externalPlugin{url: "http://203.0.113.10/"}, Options{
		Timeout: time.Second, AllowedZones: []policy.Zone{policy.LAN},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = bounded.Transform(context.Background(), "texte")
	if errs.Classify(err) != errs.PolicyBlocked {
		t.Fatalf("Transform() error = %v, class = %s", err, errs.Classify(err))
	}
}

func TestExternalCapabilityEnforcesRuntimeContext(t *testing.T) {
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
		close(canceled)
	}))
	defer server.Close()

	bounded, err := New(externalPlugin{url: server.URL}, Options{
		Timeout: 20 * time.Millisecond, AllowedZones: []policy.Zone{policy.LAN},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = bounded.Transform(context.Background(), "texte")
	if errs.Classify(err) != errs.Timeout {
		t.Fatalf("Transform() error = %v, class = %s", err, errs.Classify(err))
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP request survived the plugin timeout")
	}
}
