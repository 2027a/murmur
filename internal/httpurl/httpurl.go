package httpurl

import (
	"errors"
	"net/url"
	"strings"
)

// ResolveEndpoint preserves Murmur's endpoint semantics: root-relative paths
// start at the origin, while other relative paths are appended to base.
func ResolveEndpoint(base *url.URL, raw string) (*url.URL, error) {
	reference, err := url.Parse(raw)
	if err != nil || reference.User != nil || reference.Fragment != "" {
		return nil, errors.New("URL HTTP(S) invalide, avec userinfo ou fragment")
	}

	var resolved *url.URL
	switch {
	case reference.IsAbs() || reference.Host != "" || strings.HasPrefix(reference.Path, "/"):
		resolved = base.ResolveReference(reference)
	default:
		joined, err := url.JoinPath(base.String(), reference.Path)
		if err != nil {
			return nil, err
		}
		resolved, err = url.Parse(joined)
		if err != nil {
			return nil, err
		}
		resolved.RawQuery = reference.RawQuery
		resolved.ForceQuery = reference.ForceQuery
	}
	if resolved.Host == "" || (resolved.Scheme != "http" && resolved.Scheme != "https") ||
		resolved.User != nil || resolved.Fragment != "" {
		return nil, errors.New("URL HTTP(S) absolue requise, sans userinfo ni fragment")
	}
	return resolved, nil
}

func SameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) &&
		effectivePort(left) == effectivePort(right)
}

func effectivePort(endpoint *url.URL) string {
	if port := endpoint.Port(); port != "" {
		return port
	}
	if strings.EqualFold(endpoint.Scheme, "http") {
		return "80"
	}
	if strings.EqualFold(endpoint.Scheme, "https") {
		return "443"
	}
	return ""
}
