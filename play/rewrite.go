package play

import (
	"bufio"
	"bytes"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	uriAttr        = regexp.MustCompile(`URI="([^"]+)"`)
	resolutionAttr = regexp.MustCompile(`RESOLUTION=\d+x(\d+)`)
)

// rewrite points every URL in an m3u8 back at the proxy, so nested playlists and
// segments reach the player with the same upstream treatment
// base is the URL the playlist was ultimately served from after redirects, which
// is what relative child URIs resolve against
// height restricts a master to the variants of that picture height, applied
// before rewriting so a media playlist and a master with no such variant pass
// through whole
func (p *Proxy) rewrite(body []byte, referer string, base *url.URL, height int) []byte {
	body = filterMaster(body, height)
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

// filterMaster keeps only the variants of one picture height in a master
// playlist, every other line intact, so the EXT-X-MEDIA renditions the master
// associates stay attached to the variants that remain
// a height no variant carries filters nothing, since a master emptied of
// variants would play nothing at all where the full master still plays
func filterMaster(body []byte, height int) []byte {
	if height <= 0 || !bytes.Contains(body, []byte("#EXT-X-STREAM-INF")) {
		return body
	}
	if !hasVariant(body, height) {
		return body
	}

	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	drop := false
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "#EXT-X-STREAM-INF"):
			if drop = variantHeight(trimmed) != height; drop {
				continue
			}
		case drop && trimmed != "" && !strings.HasPrefix(trimmed, "#"):
			// the URI of the variant whose tag was dropped
			drop = false
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if sc.Err() != nil {
		return body
	}
	return out.Bytes()
}

func hasVariant(body []byte, height int) bool {
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("#EXT-X-STREAM-INF")) && variantHeight(string(trimmed)) == height {
			return true
		}
	}
	return false
}

// variantHeight reads the picture height off one EXT-X-STREAM-INF line, zero
// when the tag names no resolution
func variantHeight(line string) int {
	m := resolutionAttr.FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	h, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return h
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
	for line := range bytes.SplitSeq(body, []byte("\n")) {
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
