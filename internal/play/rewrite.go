package play

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"ysun.co/miruro/internal/upstream"
)

// errPlaylist marks a playlist the rewriter cannot take apart
// the alternative is handing the player the upstream urls, which are exactly
// what the proxy exists to keep away from it
var errPlaylist = errors.New("playlist unreadable")

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
// a playlist that cannot be scanned is refused rather than passed on with its
// upstream urls intact
func (p *Proxy) rewrite(body []byte, from target, base *url.URL) ([]byte, error) {
	body, err := filterMaster(body, from.Height)
	if err != nil {
		return nil, err
	}
	body = preferAudio(body, from.Lang)
	child := childKind(body)

	var out bytes.Buffer
	sc := lines(body)
	for sc.Scan() {
		line := sc.Text()
		switch trimmed := strings.TrimSpace(line); {
		case trimmed == "":
			out.WriteString(line)
		case strings.HasPrefix(trimmed, "#"):
			out.WriteString(p.tag(line, base, from))
		default:
			out.WriteString(p.child(trimmed, base, from, child))
		}
		out.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", errPlaylist, err)
	}
	return out.Bytes(), nil
}

// filterMaster keeps only the variants of one picture height in a master
// playlist, every other line intact, so the EXT-X-MEDIA renditions the master
// associates stay attached to the variants that remain
// a height no variant carries filters nothing, since a master emptied of
// variants would play nothing at all where the full master still plays
func filterMaster(body []byte, height int) ([]byte, error) {
	// hasVariant matches only EXT-X-STREAM-INF lines, so a body that is not a
	// master carries none and needs no separate check
	if height <= 0 || !hasVariant(body, height) {
		return body, nil
	}

	var out bytes.Buffer
	sc := lines(body)
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
			drop = false
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", errPlaylist, err)
	}
	return out.Bytes(), nil
}

// preferAudio marks the audio renditions of lang as the default of a master and
// every other one as not, so the player, ffmpeg and the segment cache all take
// the sound the provider means
// a master with no rendition in lang is left as it is, since unmarking its
// default would leave each of them to guess
func preferAudio(body []byte, lang string) []byte {
	if lang == "" || !isMaster(body) {
		return body
	}
	audio := func(line string) (bool, bool) {
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			return false, false
		}
		a := attributes(line)
		return a["TYPE"] == "AUDIO", upstream.SameLanguage(a["LANGUAGE"], lang)
	}
	meant := false
	for line := range strings.SplitSeq(string(body), "\n") {
		_, ok := audio(strings.TrimSpace(line))
		meant = meant || ok
	}
	if !meant {
		return body
	}

	var out bytes.Buffer
	sc := lines(body)
	for sc.Scan() {
		line := sc.Text()
		switch isAudio, ok := audio(strings.TrimSpace(line)); {
		case ok:
			// a rendition marked default has to be one a player may pick on its
			// own, so AUTOSELECT follows wherever the tag carries it
			line = setAttr(setAttr(line, "DEFAULT", "YES"), "AUTOSELECT", "YES")
		case isAudio:
			line = setAttr(line, "DEFAULT", "NO")
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// setAttr sets an enumerated attribute of an HLS tag, appending it when the tag
// carries none, except AUTOSELECT, which only a tag already carrying it needs
func setAttr(line, key, value string) string {
	re := enumAttrs[key]
	if re.MatchString(line) {
		return re.ReplaceAllString(line, "${1}"+key+"="+value)
	}
	if key == "AUTOSELECT" {
		return line
	}
	return strings.TrimRight(line, " \r") + "," + key + "=" + value
}

// enumAttrs finds an enumerated attribute by name, anchored on the separator so
// DEFAULT is not found inside another name
var enumAttrs = map[string]*regexp.Regexp{
	"DEFAULT":    regexp.MustCompile(`([:,])DEFAULT=[A-Z]*`),
	"AUTOSELECT": regexp.MustCompile(`([:,])AUTOSELECT=[A-Z]*`),
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

// childKind decides how the entries of a playlist are treated
// a byterange playlist addresses slices of one resource, so its entries are
// relayed with the range the player asks for rather than fetched whole, which
// forgoes the decoy strip on a shape no decoyed provider uses
func childKind(body []byte) kind {
	switch {
	case isMaster(body):
		return playlist
	case bytes.Contains(body, []byte("#EXT-X-BYTERANGE")):
		return media
	case encrypted(body):
		return cipher
	default:
		return segment
	}
}

// isMaster reports whether a playlist lists variants rather than segments
func isMaster(body []byte) bool {
	return bytes.Contains(body, []byte("#EXT-X-STREAM-INF"))
}

// lines scans a playlist with room for the longest line a real one carries
func lines(body []byte) *bufio.Scanner {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return sc
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
func (p *Proxy) tag(line string, base *url.URL, from target) string {
	loc := uriAttr.FindStringSubmatchIndex(line)
	if loc == nil {
		return line
	}
	k := opaque
	if strings.HasPrefix(line, "#EXT-X-MEDIA") || strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF") {
		k = playlist
	}
	// an audio rendition's playlist and every segment it names count as sound,
	// so a stream whose audio dies can be told from one playing with it
	if strings.HasPrefix(line, "#EXT-X-MEDIA") && strings.Contains(line, "TYPE=AUDIO") {
		from.Audio = true
	}
	return line[:loc[2]] + p.child(line[loc[2]:loc[3]], base, from, k) + line[loc[3]:]
}

// child is the payload of one url a playlist names, relayed with the referer
// and counted against the stream of the playlist that named it
func (p *Proxy) child(ref string, base *url.URL, from target, k kind) string {
	abs, err := upstream.Resolve(base.String(), ref)
	if err != nil {
		return ref
	}
	// a non-http URI such as a data key is consumed by the player directly
	// proxying it would only 502 because the upstream client speaks http
	if !strings.HasPrefix(abs, "http://") && !strings.HasPrefix(abs, "https://") {
		return ref
	}
	return p.encode(target{URL: abs, Referer: from.Referer, Kind: k, Tally: from.Tally, Audio: from.Audio})
}
