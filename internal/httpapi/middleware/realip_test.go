package middleware

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func mustPrefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatalf("ParsePrefix(%q) = %v", c, err)
		}
		out = append(out, p)
	}
	return out
}

// Rules.md §2.14: X-Forwarded-For is a claim. It counts only when the peer is
// a declared proxy, because these addresses land in audit records.
func TestRealIPIgnoresForwardedHeaderFromUntrustedPeers(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		forwarded  []string
		trusted    []string
		want       string
	}{
		{
			name:       "no trusted proxies configured, header ignored",
			remoteAddr: "203.0.113.9:1234",
			forwarded:  []string{"1.2.3.4"},
			trusted:    nil,
			want:       "203.0.113.9",
		},
		{
			name:       "peer not in trusted list, header ignored",
			remoteAddr: "203.0.113.9:1234",
			forwarded:  []string{"1.2.3.4"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "203.0.113.9",
		},
		{
			name:       "trusted peer, single hop honoured",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"198.51.100.7"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "198.51.100.7",
		},
		{
			name:       "chained proxies, rightmost untrusted hop wins",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"1.2.3.4, 198.51.100.7, 10.0.0.9"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "198.51.100.7",
		},
		{
			name:       "spoofed prefix left of the real client is discarded",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"127.0.0.1, 203.0.113.1"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "203.0.113.1",
		},
		{
			name:       "malformed hop falls back to peer",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"not-an-ip"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "10.0.0.5",
		},
		{
			name:       "all hops trusted falls back to peer",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"10.0.0.6, 10.0.0.7"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "10.0.0.5",
		},
		{
			name:       "no header at all",
			remoteAddr: "192.0.2.44:5555",
			trusted:    []string{"10.0.0.0/8"},
			want:       "192.0.2.44",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			h := RealIP(mustPrefixes(t, tc.trusted...))(
				http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					got = RealIPFrom(r.Context())
				}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			for _, f := range tc.forwarded {
				req.Header.Add("X-Forwarded-For", f)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			if got != tc.want {
				t.Errorf("client ip = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRealIPHandlesIPv6Peer(t *testing.T) {
	var got string
	h := RealIP(nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = RealIPFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "[2001:db8::1]:443"
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got != "2001:db8::1" {
		t.Errorf("client ip = %q, want 2001:db8::1", got)
	}
}
