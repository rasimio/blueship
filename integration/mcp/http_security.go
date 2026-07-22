package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

type netIPResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type contextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

func newMCPHTTPClient(authHeader string) *http.Client {
	// A proxy would resolve and connect to the target itself, bypassing the
	// verified-IP dial below. MCP endpoints therefore always dial directly.
	base := &http.Transport{
		Proxy: nil,
		DialContext: safeDialContext(net.DefaultResolver, &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Transport:     base,
		Timeout:       60 * time.Second,
		CheckRedirect: mcpRedirectPolicy(authHeader),
	}
}

// safeDialContext resolves the request hostname itself, rejects unsafe
// resolution sets, then dials a verified IP literal. The OS dialer never sees
// the hostname, so a second DNS answer cannot rebind the connection.
func safeDialContext(resolver netIPResolver, dialer contextDialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("mcp http dial address %q: %w", address, err)
		}

		var addresses []netip.Addr
		if literal, err := netip.ParseAddr(host); err == nil {
			addresses = []netip.Addr{literal}
		} else {
			lookupNetwork := "ip"
			switch network {
			case "tcp4":
				lookupNetwork = "ip4"
			case "tcp6":
				lookupNetwork = "ip6"
			}
			addresses, err = resolver.LookupNetIP(ctx, lookupNetwork, host)
			if err != nil {
				return nil, fmt.Errorf("mcp http resolve %q: %w", host, err)
			}
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("mcp http resolve %q: no addresses", host)
		}
		// Reject the whole DNS answer when any address is unsafe. This avoids
		// ambiguous mixed public/private answers and keeps the policy fail-closed.
		for _, resolved := range addresses {
			if blockedMCPAddress(resolved) {
				return nil, fmt.Errorf("mcp http resolve %q: forbidden address %s", host, resolved)
			}
		}

		var dialErrors []error
		for _, resolved := range addresses {
			verifiedAddress := net.JoinHostPort(resolved.String(), port)
			conn, err := dialer.DialContext(ctx, network, verifiedAddress)
			if err == nil {
				return conn, nil
			}
			dialErrors = append(dialErrors, err)
		}
		return nil, fmt.Errorf("mcp http dial %q: %w", host, errors.Join(dialErrors...))
	}
}

func blockedMCPAddress(address netip.Addr) bool {
	if !address.IsValid() {
		return true
	}
	address = address.Unmap()
	return !address.IsGlobalUnicast() || address.IsPrivate() || cgnatPrefix.Contains(address)
}

func mcpRedirectPolicy(authHeader string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("mcp http: stopped after 10 redirects")
		}
		if len(via) == 0 {
			return nil
		}
		previous := via[len(via)-1].URL
		if strings.EqualFold(previous.Scheme, "https") && strings.EqualFold(req.URL.Scheme, "http") {
			return fmt.Errorf("mcp http: refusing HTTPS to HTTP redirect from %s to %s", previous.Redacted(), req.URL.Redacted())
		}
		if !sameOrigin(via[0].URL, req.URL) {
			for _, header := range []string{authHeader, "Authorization", "Proxy-Authorization", "Cookie", "Mcp-Session-Id"} {
				if header != "" {
					req.Header.Del(header)
				}
			}
		}
		return nil
	}
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		originPort(a) == originPort(b)
}

func originPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}
