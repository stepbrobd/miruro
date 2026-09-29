// Package upstream is the vocabulary every backend speaks: the title, episode,
// stream, and subtitle records, the Backend interface an upstream implements,
// the merge of several backends into one catalog, and the heuristics that
// pick a stream and a subtitle track out of what they answer.
// The backends themselves live beside it, one package per upstream.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// UserAgent is the one browser identity shared by every backend, the quality
// probe, and the stream proxy, so a CDN sees a single client across a
// playlist and its segments
const UserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"

var (
	// ErrBlocked is fatal for the backend that raised it
	// its firewall rejected this client, and every further request would meet
	// the same rejection
	ErrBlocked = errors.New("cloudflare blocked request")
	// ErrUnreachable is recoverable and drives provider fallback
	ErrUnreachable = errors.New("upstream unreachable")
	// ErrPrivate marks an address no provider has any business naming, a
	// loopback, private, link-local or shared one
	ErrPrivate = errors.New("non-public address")
)

// shared is the carrier-grade NAT range, which a tailnet also lives in, so it
// is as private to this machine as a LAN
var shared = netip.MustParsePrefix("100.64.0.0/10")

// Public returns the client that fetches what a provider names: a stream, a
// playlist, a segment, a key or a subtitle
// every such url comes from a provider, so a dial to anywhere but the public
// internet is refused, and the check runs on the address being dialed, after
// resolution and on every redirect, so a name resolving to a LAN host is
// refused as well
// a proxy the environment configures is the one such address it may dial,
// since every request goes through it
// the header wait is bounded so a stalled CDN cannot wedge a fetch, and the
// body is not, since the stream proxy relays a whole mp4 through this client
func Public() *http.Client {
	allowed := proxies()
	d := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			return dialable(address, allowed)
		},
	}
	// the cloned default transport keeps HTTP/2 through ALPN, which kiwi's CDN
	// needs, and keeps it with a custom dialer since the default forces the attempt
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = d.DialContext
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: tr}
}

// dialable refuses an address outside the public internet unless it is one
// the environment named as a proxy
func dialable(address string, allowed map[netip.AddrPort]bool) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPrivate, err)
	}
	ip := ap.Addr().Unmap()
	if allowed[netip.AddrPortFrom(ip, ap.Port())] || public(ip) {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrPrivate, ip)
}

// public reports whether ip is a unicast address on the public internet
func public(ip netip.Addr) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !shared.Contains(ip)
}

// proxies is every address the environment's http and https proxies resolve
// to, which a guarded dial must still reach
// net/http reads the environment once per process, so this reads it the same
// way rather than parsing the variables again
func proxies() map[netip.AddrPort]bool {
	return proxiesFrom(http.ProxyFromEnvironment)
}

// proxiesFrom is every address the proxies proxy names for http and https
// resolve to, apart from where it reads them so a test can name its own
func proxiesFrom(proxy func(*http.Request) (*url.URL, error)) map[netip.AddrPort]bool {
	out := map[netip.AddrPort]bool{}
	ports := map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}
	for _, scheme := range []string{"http", "https"} {
		u, err := proxy(&http.Request{URL: &url.URL{Scheme: scheme, Host: "example.com"}})
		if err != nil || u == nil {
			continue
		}
		// a proxy url without a port takes its own scheme's, not the target's
		p := u.Port()
		if p == "" {
			p = ports[u.Scheme]
		}
		ips, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip", u.Hostname())
		if err != nil {
			continue
		}
		for _, ip := range ips {
			if ap, err := netip.ParseAddrPort(net.JoinHostPort(ip.Unmap().String(), p)); err == nil {
				out[ap] = true
			}
		}
	}
	return out
}

// SetReferer attaches the referer a provider named and the origin it belongs to
// a browser player sends both on every cross-origin media fetch, and a CDN that
// keys on the pair answers 403 to a request carrying only the referer
// hop's segment hosts do exactly that, so without the origin its streams
// resolve, list their segments, and never relay one
func SetReferer(h http.Header, referer string) {
	if referer == "" {
		return
	}
	h.Set("Referer", referer)
	u, err := url.Parse(referer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return
	}
	h.Set("Origin", u.Scheme+"://"+u.Host)
}

func newGet(ctx context.Context, url, referer string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	SetReferer(req.Header, referer)
	return req, nil
}
