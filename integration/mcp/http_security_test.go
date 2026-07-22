package mcp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

type staticNetIPResolver struct {
	addresses []netip.Addr
	err       error
}

func (r staticNetIPResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r.addresses, r.err
}

type recordingContextDialer struct {
	addresses []string
}

func (d *recordingContextDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.addresses = append(d.addresses, address)
	return nil, errors.New("test dial stopped")
}

func TestBlockedMCPAddress(t *testing.T) {
	tests := []struct {
		address string
		blocked bool
	}{
		{"8.8.8.8", false},
		{"100.128.0.1", false},
		{"2606:4700:4700::1111", false},
		{"127.0.0.1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.1.1", true},
		{"0.0.0.0", true},
		{"224.0.0.1", true},
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"::", true},
		{"::1", true},
		{"fe80::1", true},
		{"ff02::1", true},
		{"fc00::1", true},
		{"fd12:3456::1", true},
		{"::ffff:127.0.0.1", true},
		{"::ffff:10.0.0.1", true},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			address := netip.MustParseAddr(test.address)
			if got := blockedMCPAddress(address); got != test.blocked {
				t.Fatalf("blockedMCPAddress(%s) = %v, want %v", address, got, test.blocked)
			}
		})
	}
}

func TestNewMCPHTTPClientUsesOnlySafeDirectDial(t *testing.T) {
	client := newMCPHTTPClient("Authorization")
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("MCP HTTP transport must not use environment proxies")
	}
	if transport.DialContext == nil || transport.DialTLSContext != nil {
		t.Fatal("HTTPS must flow through the verified-IP DialContext")
	}
	if client.CheckRedirect == nil {
		t.Fatal("redirect policy is missing")
	}
}

func TestSafeDialContextRejectsLiteralBeforeDial(t *testing.T) {
	dial := safeDialContext(net.DefaultResolver, &net.Dialer{})
	conn, err := dial(context.Background(), "tcp", "127.0.0.1:8080")
	if conn != nil {
		conn.Close()
		t.Fatal("loopback dial unexpectedly returned a connection")
	}
	if err == nil || !strings.Contains(err.Error(), "forbidden address") {
		t.Fatalf("error = %v, want forbidden address", err)
	}
}

func TestSafeDialContextPinsVerifiedResolution(t *testing.T) {
	t.Run("dials resolved IP literal", func(t *testing.T) {
		resolver := staticNetIPResolver{addresses: []netip.Addr{netip.MustParseAddr("8.8.8.8")}}
		dialer := &recordingContextDialer{}
		_, err := safeDialContext(resolver, dialer)(context.Background(), "tcp", "public.example:443")
		if err == nil {
			t.Fatal("fake dial should return an error")
		}
		if len(dialer.addresses) != 1 || dialer.addresses[0] != "8.8.8.8:443" {
			t.Fatalf("dial addresses = %v, want verified IP literal", dialer.addresses)
		}
	})

	t.Run("mixed public private answer fails closed", func(t *testing.T) {
		resolver := staticNetIPResolver{addresses: []netip.Addr{
			netip.MustParseAddr("8.8.8.8"),
			netip.MustParseAddr("127.0.0.1"),
		}}
		dialer := &recordingContextDialer{}
		_, err := safeDialContext(resolver, dialer)(context.Background(), "tcp", "mixed.example:443")
		if err == nil || !strings.Contains(err.Error(), "forbidden address") {
			t.Fatalf("mixed resolution error = %v", err)
		}
		if len(dialer.addresses) != 0 {
			t.Fatalf("mixed resolution reached dialer: %v", dialer.addresses)
		}
	})
}

func TestMCPRedirectPolicy(t *testing.T) {
	request := func(rawURL string) *http.Request {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{URL: u, Header: http.Header{
			"Authorization":  []string{"Bearer secret"},
			"X-Api-Key":      []string{"secret"},
			"Cookie":         []string{"session=secret"},
			"Mcp-Session-Id": []string{"mcp-session"},
		}}
	}
	policy := mcpRedirectPolicy("X-Api-Key")

	t.Run("same origin preserves auth", func(t *testing.T) {
		original := request("https://example.com/start")
		next := request("https://example.com:443/next")
		if err := policy(next, []*http.Request{original}); err != nil {
			t.Fatalf("redirect: %v", err)
		}
		if next.Header.Get("Authorization") == "" || next.Header.Get("X-Api-Key") == "" {
			t.Fatal("same-origin auth was stripped")
		}
	})

	t.Run("cross origin strips auth and session", func(t *testing.T) {
		original := request("https://example.com/start")
		next := request("https://other.example/next")
		if err := policy(next, []*http.Request{original}); err != nil {
			t.Fatalf("redirect: %v", err)
		}
		for _, header := range []string{"Authorization", "X-Api-Key", "Cookie", "Mcp-Session-Id"} {
			if value := next.Header.Get(header); value != "" {
				t.Fatalf("cross-origin %s leaked: %q", header, value)
			}
		}
	})

	t.Run("different port is different origin", func(t *testing.T) {
		original := request("https://example.com/start")
		next := request("https://example.com:8443/next")
		if err := policy(next, []*http.Request{original}); err != nil {
			t.Fatalf("redirect: %v", err)
		}
		if next.Header.Get("Authorization") != "" {
			t.Fatal("authorization survived a port change")
		}
	})

	t.Run("https downgrade rejected", func(t *testing.T) {
		original := request("https://example.com/start")
		next := request("http://example.com/next")
		if err := policy(next, []*http.Request{original}); err == nil || !strings.Contains(err.Error(), "HTTPS to HTTP") {
			t.Fatalf("downgrade error = %v", err)
		}
	})
}
