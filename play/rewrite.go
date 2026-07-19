package play

import (
	"bufio"
	"bytes"
	"net/url"
	"regexp"
	"strings"
)

var uriAttr = regexp.MustCompile(`URI="([^"]+)"`)

// rewrite points every URL in an m3u8 back at the proxy, so nested playlists and
// segments reach the player with the same upstream treatment
// base is the URL the playlist was ultimately served from after redirects, which
// is what relative child URIs resolve against
func (p *Proxy) rewrite(body []byte, referer string, base *url.URL) []byte {
	child := childKind(body)

	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch trimmed := strings.TrimSpace(line); {
		case trimmed == "":
			out.WriteString(line)
		case strings.HasPrefix(trimmed, "#"):
			out.WriteString(p.tag(line, base, referer))
		default:
			out.WriteString(p.child(trimmed, base, referer, child))
		}
		out.WriteByte('\n')
	}
	if sc.Err() != nil {
		return body
	}
	return out.Bytes()
}

func childKind(body []byte) kind {
	switch {
	case bytes.Contains(body, []byte("#EXT-X-STREAM-INF")):
		return playlist
	case encrypted(body):
		return cipher
	default:
		return segment
	}
}

// encrypted reports whether the playlist declares a key, which makes its
// segments ciphertext that has to reach the player byte for byte
func encrypted(body []byte) bool {
	for _, line := range bytes.Split(body, []byte("\n")) {
		if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("#EXT-X-KEY")) {
			continue
		}
		if !bytes.Contains(line, []byte("METHOD=NONE")) {
			return true
		}
	}
	return false
}

// tag rewrites a URI attribute
// EXT-X-MEDIA and EXT-X-I-FRAME-STREAM-INF both name a media playlist whatever
// the URI looks like, every other tag URI is data the player consumes directly
func (p *Proxy) tag(line string, base *url.URL, referer string) string {
	loc := uriAttr.FindStringSubmatchIndex(line)
	if loc == nil {
		return line
	}
	k := opaque
	if strings.HasPrefix(line, "#EXT-X-MEDIA") || strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF") {
		k = playlist
	}
	return line[:loc[2]] + p.child(line[loc[2]:loc[3]], base, referer, k) + line[loc[3]:]
}

func (p *Proxy) child(ref string, base *url.URL, referer string, k kind) string {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ref
	}
	abs := base.ResolveReference(u)
	// a non-http URI such as a data key is consumed by the player directly
	// proxying it would only 502 because the upstream client speaks http
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return ref
	}
	return p.proxied(abs.String(), referer, k)
}
