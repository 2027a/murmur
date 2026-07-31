package policy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"

	"git.2027a.net/mathieu/murmur/internal/errs"
)

func TestClassify(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}
	tests := []struct {
		ip   string
		want Zone
	}{
		{"127.0.0.1", Local},
		{"::1", Local},
		{"10.20.30.40", LAN},
		{"172.16.0.1", LAN},
		{"192.168.1.1", LAN},
		{"fd00::1", LAN},
		{"169.254.10.20", LAN},
		{"fe80::1", LAN},
		{"198.51.100.42", LAN},
		{"8.8.8.8", Internet},
		{"2001:4860:4860::8888", Internet},
	}
	for _, test := range tests {
		t.Run(test.ip, func(t *testing.T) {
			if got := Classify(net.ParseIP(test.ip), trusted); got != test.want {
				t.Fatalf("Classify(%s) = %s, want %s", test.ip, got, test.want)
			}
		})
	}
}

type countingListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return conn, err
}

func TestTransportAllowsLoopback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := &http.Client{Transport: Transport(Local)}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

func TestTransportBlocksBeforeConnection(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not be reached")
	}))
	listener := &countingListener{Listener: server.Listener}
	server.Listener = listener
	server.Start()
	defer server.Close()

	// A deliberately invalid lower-than-local policy rejects even loopback.
	transport := Transport(Local)
	transport.DialContext = guardedDialer(Zone(-1), nil)
	client := &http.Client{Transport: transport}
	_, err := client.Get(server.URL)
	if errs.Classify(err) != errs.PolicyBlocked {
		t.Fatalf("error = %v, class = %s", err, errs.Classify(err))
	}
	if got := listener.accepted.Load(); got != 0 {
		t.Fatalf("server accepted %d connections, want 0", got)
	}
}

func TestRedirectIsRechecked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://203.0.113.10/next", http.StatusFound)
	}))
	defer server.Close()
	client := &http.Client{Transport: Transport(Local)}
	_, err := client.Get(server.URL)
	if errs.Classify(err) != errs.PolicyBlocked {
		t.Fatalf("redirect error = %v, class = %s", err, errs.Classify(err))
	}
}
