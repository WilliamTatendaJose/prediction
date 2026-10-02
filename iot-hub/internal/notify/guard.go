package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync/atomic"
	"syscall"
	"time"
)

// In a multi-tenant hub, tenants configure where notifications go. Without
// a guard a tenant could aim a "webhook" at the platform's own network
// (cloud metadata at 169.254.169.254, databases, admin panels) and use the
// hub to reach it (server-side request forgery).
var blockPrivate atomic.Bool

// BlockPrivateNetworks makes every notification connection refuse
// loopback, private, link-local, CGNAT and other non-public addresses.
func BlockPrivateNetworks(on bool) { blockPrivate.Store(on) }

// ErrBlocked: the destination resolves to a non-public address.
var ErrBlocked = errors.New("destination is not a public address (blocked in multi-tenant mode)")

var extraBlocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT, also some cloud internals
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can map to private IPv4
}

func blocked(a netip.Addr) bool {
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range extraBlocked {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// control runs after DNS resolution, on the address actually dialled, so a
// name that resolves (or re-resolves) to a private address is caught too.
func control(_, address string, _ syscall.RawConn) error {
	if !blockPrivate.Load() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	a, err := netip.ParseAddr(host)
	if err != nil || blocked(a) {
		return fmt.Errorf("%w: %s", ErrBlocked, host)
	}
	return nil
}

func dialer() *net.Dialer {
	return &net.Dialer{Timeout: 15 * time.Second, Control: control}
}

// httpClient: with the guard on, connections go direct (an HTTP proxy
// would make the dial check meaningless).
func httpClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer().DialContext(ctx, network, addr)
	}
	tr.Proxy = func(r *http.Request) (*url.URL, error) {
		if blockPrivate.Load() {
			return nil, nil
		}
		return http.ProxyFromEnvironment(r)
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: tr}
}

// checkHost rejects obviously private destinations when a target is
// configured, for a clear error up front (the dial check is the real one).
func checkHost(host string) error {
	if !blockPrivate.Load() {
		return nil
	}
	if host == "localhost" || len(host) > 10 && host[len(host)-10:] == ".localhost" {
		return ErrBlocked
	}
	if a, err := netip.ParseAddr(host); err == nil && blocked(a) {
		return ErrBlocked
	}
	return nil
}
