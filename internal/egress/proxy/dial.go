// Package proxy implements the M3a build proxy: strict CONNECT parsing,
// allowlisting, in-proxy DNS resolution and SSRF-safe dialing.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/jerryjuche/padi/internal/egress/config"
)

// Resolver is the DNS interface used by the proxy and research fetcher. Tests
// inject a fake resolver; production uses net.DefaultResolver.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// DialIPFunc dials an already checked IP. It must never resolve the host again.
type DialIPFunc func(ctx context.Context, network string, ip net.IP, port string) (net.Conn, error)

// DefaultDialIP is the production dialer.
func DefaultDialIP(ctx context.Context, network string, ip net.IP, port string) (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
}

// GuardedDialer resolves DNS itself, calls CheckIP on every resolved address,
// and dials only an address that has been checked. No second lookup occurs.
type GuardedDialer struct {
	Resolver Resolver
	DialIP   DialIPFunc
	Ports    []int
	DenyNets []*net.IPNet
	// OnDeny receives a stable reason code, never a URL or query value.
	OnDeny func(host, reason string)
}

func (d *GuardedDialer) deny(host, reason string) error {
	if d != nil && d.OnDeny != nil {
		d.OnDeny(host, reason)
	}
	return &config.DenyError{Reason: reason}
}

// CheckedAddresses resolves host and returns every address only after all of
// them have passed CheckIP. A single bad address rejects the whole host.
func (d *GuardedDialer) CheckedAddresses(ctx context.Context, host, port string) ([]net.IP, error) {
	if d == nil {
		return nil, errors.New("nil guarded dialer")
	}
	host = config.CanonicalHost(host)
	if net.ParseIP(host) != nil {
		return nil, d.deny(host, "ip_literal")
	}
	if err := config.ValidateHost(host); err != nil {
		return nil, d.deny(host, "bad_host")
	}
	n, err := config.ParsePort(port)
	if err != nil || !config.PortAllowed(n, d.Ports) {
		return nil, d.deny(host, "port_not_allowed")
	}
	resolver := d.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", config.ErrResolve, host, err)
	}
	if len(addrs) == 0 {
		return nil, d.deny(host, "no_addresses")
	}
	checked := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		ip := addr.IP
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		v := config.CheckIPIn(ip, d.DenyNets)
		if !v.Allow {
			return nil, d.deny(host, v.Reason)
		}
		checked = append(checked, ip)
	}
	return checked, nil
}

// DialChecked dials an address from CheckedAddresses. It retries other checked
// addresses on connection failure, but never resolves the hostname again.
func (d *GuardedDialer) DialChecked(ctx context.Context, network string, ips []net.IP, port string) (net.Conn, error) {
	if d == nil || len(ips) == 0 {
		return nil, errors.New("no checked addresses")
	}
	dial := d.DialIP
	if dial == nil {
		dial = DefaultDialIP
	}
	var errs []error
	for _, ip := range ips {
		conn, err := dial(ctx, network, ip, port)
		if err == nil {
			return conn, nil
		}
		errs = append(errs, fmt.Errorf("dial %s: %w", ip.String(), err))
	}
	return nil, errors.Join(errs...)
}

// DialContext implements the http.Transport dial hook.
func (d *GuardedDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := config.ParseHostPort(addr)
	if err != nil {
		return nil, d.deny(addr, "bad_host")
	}
	ips, err := d.CheckedAddresses(ctx, host, port)
	if err != nil {
		return nil, err
	}
	return d.DialChecked(ctx, network, ips, port)
}
