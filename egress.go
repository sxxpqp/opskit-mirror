package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"time"
)

var errNonPublicAddress = errors.New("destination address is not public")

// These ranges are not public CDN destinations, even where IsGlobalUnicast
// considers them unicast (shared, documentation, benchmark and transition ranges).
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("168.63.129.16/32"), // Azure platform virtual IP.
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Zone() != "" {
		return false
	}
	// Exclude local, translation and other non-global IPv6 address spaces.
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func publicRedirectURL(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return false
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		return publicAddress(ip)
	}
	return true // DNS answers are validated and pinned when opening the socket.
}

func publicDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	return dialPublic(ctx, network, address, net.DefaultResolver.LookupNetIP, dialer.DialContext)
}

func dialPublic(ctx context.Context, network, address string,
	lookup func(context.Context, string, string) ([]netip.Addr, error),
	dial func(context.Context, string, string) (net.Conn, error),
) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = lookup(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("destination has no addresses")
	}
	// Reject mixed public/private DNS responses before opening any connection.
	for _, ip := range ips {
		if !publicAddress(ip) {
			return nil, errNonPublicAddress
		}
	}
	for _, ip := range ips {
		// Dial the validated IP, never the hostname, to prevent DNS rebinding.
		conn, dialErr := dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		err = dialErr
	}
	return nil, err
}
