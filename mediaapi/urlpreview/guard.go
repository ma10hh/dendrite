// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package urlpreview

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// ErrBlocked is returned for addresses the server must not connect to on behalf of users.
var ErrBlocked = errors.New("the address is not allowed")

// alwaysDenied are the addresses that mean this host or its network rather than the internet: loopback, private,
// link local (which includes the metadata services of cloud providers), shared and reserved ranges.
var alwaysDenied = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8", "64:ff9b:1::/48",
)

// The IPv6 ranges that carry an IPv4 address: it is checked instead of the IPv6 address.
var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
	v4Compat  = netip.MustParsePrefix("::/96")
)

func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		out[i] = netip.MustParsePrefix(c)
	}
	return out
}

// ParsePrefixes parses CIDR ranges, or single addresses, from the configuration.
func ParsePrefixes(values []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(values))
	for _, v := range values {
		if p, err := netip.ParsePrefix(v); err == nil {
			out = append(out, p.Masked())
		} else if a, aerr := netip.ParseAddr(v); aerr == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		} else {
			return nil, fmt.Errorf("%q is not an IP address or a CIDR range", v)
		}
	}
	return out, nil
}

// Guard decides which addresses may be connected to.
type Guard struct {
	// Deny is denied in addition to the ranges that are always denied.
	Deny []netip.Prefix
	// Allow lifts the denial of the ranges it covers, for hosts the administrator trusts.
	Allow []netip.Prefix
}

func contains(prefixes []netip.Prefix, a netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Allowed reports whether a connection to the address is acceptable.
func (g *Guard) Allowed(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if contains(g.Allow, a) {
		return true
	}
	if contains(alwaysDenied, a) || contains(g.Deny, a) {
		return false
	}
	switch {
	case nat64.Contains(a), v4Compat.Contains(a):
		raw := a.As16()
		return g.Allowed(netip.AddrFrom4([4]byte{raw[12], raw[13], raw[14], raw[15]}))
	case sixToFour.Contains(a):
		raw := a.As16()
		return g.Allowed(netip.AddrFrom4([4]byte{raw[2], raw[3], raw[4], raw[5]}))
	}
	return true
}

// DialContext connects to a host, but only to addresses that pass the guard. The names are resolved here and the
// connection is made to the address that was checked, so that a name that answers differently the second time
// cannot lead somewhere else.
func (g *Guard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	var candidates []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		candidates = []netip.Addr{ip}
	} else {
		resolved, rerr := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if rerr != nil {
			return nil, rerr
		}
		candidates = resolved
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	var lastErr error = ErrBlocked
	for _, ip := range candidates {
		if !g.Allowed(ip) {
			continue
		}
		conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	return nil, lastErr
}
