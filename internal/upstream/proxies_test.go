package upstream

import (
	"errors"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"testing"
)

// a proxy the environment names is reached whatever range it sits in, on the
// port it names or its own scheme's default, while a scheme with no proxy, one
// that cannot be read and a host that does not resolve add nothing
func TestProxiesFrom(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proxy func(*http.Request) (*url.URL, error)
		want  []string
	}{
		{"a port named", func(*http.Request) (*url.URL, error) {
			return url.Parse("http://10.0.0.5:3128")
		}, []string{"10.0.0.5:3128"}},
		{"a scheme's own port", func(r *http.Request) (*url.URL, error) {
			if r.URL.Scheme == "https" {
				return url.Parse("socks5://192.168.1.2")
			}
			return url.Parse("http://[fd00::1]")
		}, []string{"192.168.1.2:1080", "[fd00::1]:80"}},
		{"none for either scheme", func(*http.Request) (*url.URL, error) { return nil, nil }, nil},
		{"a proxy setting that cannot be read", func(*http.Request) (*url.URL, error) {
			return nil, errors.New("invalid proxy address")
		}, nil},
		{"a host that does not resolve", func(*http.Request) (*url.URL, error) {
			return url.Parse("http://proxy.invalid:3128")
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := map[netip.AddrPort]bool{}
			for _, s := range tc.want {
				want[netip.MustParseAddrPort(s)] = true
			}
			if got := proxiesFrom(tc.proxy); !maps.Equal(got, want) {
				t.Errorf("proxiesFrom = %v, want %v", got, want)
			}
		})
	}
}
