package middleware

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// RealIP resolves the client address. Rules.md §2.14: X-Forwarded-For is read
// only when the immediate peer is inside the configured trusted CIDR list.
// With an empty list the header is ignored entirely, because these addresses
// are written to audit records and a spoofable source is a forged trail.
func RealIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r, trusted)
			ctx := context.WithValue(r.Context(), ctxKeyRealIP, ip)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func clientIP(r *http.Request, trusted []netip.Prefix) string {
	peer := peerAddr(r.RemoteAddr)
	if len(trusted) == 0 || !inAny(peer, trusted) {
		return peer.String()
	}

	// Walk X-Forwarded-For right to left, skipping addresses that are
	// themselves trusted proxies. The first untrusted hop is the client;
	// everything left of it is attacker-controlled and must be discarded.
	fwd := r.Header.Values("X-Forwarded-For")
	var hops []string
	for _, h := range fwd {
		for _, part := range strings.Split(h, ",") {
			if p := strings.TrimSpace(part); p != "" {
				hops = append(hops, p)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(hops[i])
		if err != nil {
			// A malformed hop means the chain cannot be trusted from
			// here left. Stop and use what we have.
			break
		}
		if !inAny(addr, trusted) {
			return addr.String()
		}
	}

	if xr := strings.TrimSpace(r.Header.Get("X-Real-Ip")); xr != "" {
		if addr, err := netip.ParseAddr(xr); err == nil {
			return addr.String()
		}
	}
	return peer.String()
}

func peerAddr(remoteAddr string) netip.Addr {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

func inAny(addr netip.Addr, prefixes []netip.Prefix) bool {
	if !addr.IsValid() {
		return false
	}
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
