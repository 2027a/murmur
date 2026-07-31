package policy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"

	"git.2027a.net/mathieu/murmur/internal/errs"
)

type Zone int8

const (
	Local Zone = iota
	LAN
	Internet
)

func (z Zone) String() string {
	switch z {
	case Local:
		return "local"
	case LAN:
		return "lan"
	case Internet:
		return "internet"
	default:
		return "unknown"
	}
}

func ParseZone(value string) (Zone, error) {
	switch value {
	case "local":
		return Local, nil
	case "lan":
		return LAN, nil
	case "internet":
		return Internet, nil
	default:
		return 0, errs.New(errs.ConfigInvalid, "policy", fmt.Sprintf("zone inconnue: %q", value))
	}
}

func Classify(ip net.IP, trusted []netip.Prefix) Zone {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return Internet
	}
	addr = addr.Unmap()
	if addr.IsLoopback() {
		return Local
	}
	if addr.IsPrivate() || addr.IsLinkLocalUnicast() {
		return LAN
	}
	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return LAN
		}
	}
	return Internet
}

// Transport returns a guarded HTTP transport without additional trusted CIDRs.
func Transport(allowed Zone) *http.Transport {
	return TransportWithTrusted(allowed, nil)
}

func TransportWithTrusted(allowed Zone, trusted []netip.Prefix) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = guardedDialer(allowed, append([]netip.Prefix(nil), trusted...))
	return transport
}

func guardedDialer(allowed Zone, trusted []netip.Prefix) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errs.Wrap(errs.Unavailable, "netguard", "adresse de destination invalide", err)
		}
		addresses, err := resolve(ctx, host)
		if err != nil {
			return nil, errs.Wrap(errs.Unavailable, "netguard", "résolution DNS impossible", err)
		}
		for _, candidate := range addresses {
			zone := Classify(net.IP(candidate.AsSlice()), trusted)
			if zone > allowed {
				return nil, errs.New(errs.PolicyBlocked, "netguard", fmt.Sprintf("destination %s interdite par la policy %s", zone, allowed))
			}
		}

		var lastErr error
		for _, candidate := range addresses {
			target := net.JoinHostPort(candidate.String(), port)
			conn, err := dialer.DialContext(ctx, network, target)
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, errs.Wrap(errs.Unavailable, "netguard", "connexion à la destination impossible", lastErr)
	}
}

func resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{address.Unmap()}, nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("aucune adresse pour %s", strconv.Quote(host))
	}
	for i := range addresses {
		addresses[i] = addresses[i].Unmap()
	}
	return addresses, nil
}
