package server

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// ParseTrustedProxies parses CIDRs (or bare IPs, as /32 or /128) of reverse
// proxies whose X-Forwarded-For is trusted. Blank entries are skipped. An
// invalid entry is an error naming it.
func ParseTrustedProxies(cidrs []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if p, err := netip.ParsePrefix(c); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(c)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q: want a CIDR such as 10.0.0.0/8", c)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	a = a.Unmap()
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientIP returns the client address for rate limiting (R41). It is the
// immediate peer (RemoteAddr without port) unless that peer is a trusted
// proxy; then X-Forwarded-For is walked right to left and the first entry
// that is not itself trusted wins. A missing or malformed header, or one
// whose entries are all trusted, falls back to the peer. X-Forwarded-For
// from an untrusted peer is never read.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	peer := remoteIP(r.RemoteAddr)
	if len(trusted) == 0 {
		return peer
	}
	pa, err := netip.ParseAddr(peer)
	if err != nil || !isTrusted(pa, trusted) {
		return peer
	}
	var entries []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		entries = append(entries, strings.Split(v, ",")...)
	}
	// Parse everything first: any malformed entry makes the header untrustworthy.
	addrs := make([]netip.Addr, 0, len(entries))
	for _, e := range entries {
		a, err := netip.ParseAddr(strings.TrimSpace(e))
		if err != nil {
			return peer
		}
		addrs = append(addrs, a.Unmap())
	}
	for i := len(addrs) - 1; i >= 0; i-- {
		if !isTrusted(addrs[i], trusted) {
			return addrs[i].String()
		}
	}
	return peer
}

// publicURLInsecure reports whether publicURL is not https while listen is
// not a loopback address, so session cookies go out without Secure to a
// non-local audience.
func publicURLInsecure(publicURL, listen string) bool {
	if u, err := url.Parse(publicURL); err == nil && strings.EqualFold(u.Scheme, "https") {
		return false
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	if host == "localhost" {
		return false
	}
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err != nil || !a.IsLoopback()
}
