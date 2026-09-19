package allanime

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/log"

	"ysun.co/miruro"
)

// source is one host the api lists for an episode
type source struct {
	URL      string  `json:"sourceUrl"`
	Name     string  `json:"sourceName"`
	Priority float64 `json:"priority"`
	// Type is player for a url a video element can open and iframe for an
	// embedded page
	Type      string `json:"type"`
	Extension string `json:"fileExtenstion"`
	FallBack  string `json:"fallBack"`
}

// vidInfo describes the site's own encode of an episode
type vidInfo struct {
	Resolution int `json:"vidResolution"`
}

// streams reads an opened episode answer into a result
// a player source names a url outright, a clock source names an encoded path
// the site asks for its own storage, and the rest are embedded pages nothing
// here plays, which are listed as embeds so the provider reads as resolved
// rather than empty
// the clock paths are decoded here and resolved by expandClocks afterward,
// since resolving one costs a request
func streams(plain []byte, cat miruro.Category, referer, clock string) (*miruro.Result, error) {
	var raw struct {
		Episode struct {
			Sources []source `json:"sourceUrls"`
			Info    struct {
				Sub *vidInfo `json:"vidInforssub"`
				Dub *vidInfo `json:"vidInforsdub"`
			} `json:"episodeInfo"`
		} `json:"episode"`
	}
	if err := json.Unmarshal(plain, &raw); err != nil {
		return nil, fmt.Errorf("%w: allanime episode: %v", miruro.ErrUpstream, err)
	}
	info := raw.Episode.Info.Sub
	if cat == miruro.Dub {
		info = raw.Episode.Info.Dub
	}

	srcs := raw.Episode.Sources
	slices.SortStableFunc(srcs, func(a, b source) int { return cmp.Compare(b.Priority, a.Priority) })
	res := &miruro.Result{}
	lead := true
	for _, s := range srcs {
		st, ok := stream(s, referer, clock)
		if !ok {
			continue
		}
		if st.Kind != miruro.Embed {
			if info != nil && info.Resolution > 0 {
				st.Quality = strconv.Itoa(info.Resolution) + "p"
			}
			st.Default, lead = lead, false
		}
		res.Streams = append(res.Streams, st)
	}
	return res, nil
}

// stream maps one source to a stream, refusing a url that is not http
func stream(s source, referer, clock string) (miruro.Stream, bool) {
	raw := s.URL
	if strings.HasPrefix(raw, "--") {
		raw = clock + decode(raw[2:])
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return miruro.Stream{}, false
	}
	st := miruro.Stream{URL: u.String(), Kind: miruro.Embed, Referer: referer, Server: s.Name}
	if s.Type != "player" {
		return st, true
	}
	switch container := strings.ToLower(cmp.Or(s.Extension, s.FallBack)); container {
	case "mp4":
		st.Kind = miruro.MP4
	case "m3u8", "hls":
		st.Kind = miruro.HLS
	default:
		return st, true
	}
	return st, true
}

// defaultClock serves the encoded source paths the api hands out, and clockPath
// is the endpoint they address
// the site serves an html player at the path itself, which is what answered 404
// and stalled in earlier runs, and the links behind it under the same path with
// a .json suffix, which is the only one worth asking
const (
	defaultClock = "https://allanime.day"
	clockPath    = "/apivtwo/clock"
)

// maxLinks bounds what a clock answer is trusted to carry
// a real one holds a single master, so only a broken or hostile answer is near
// this
const maxLinks = 32

// link is one playable url a clock endpoint answers
type link struct {
	Link string `json:"link"`
	HLS  bool   `json:"hls"`
	Mp4  bool   `json:"mp4"`
	// Resolution labels a direct file by height and a master as "Hls"
	Resolution string `json:"resolutionStr"`
}

// clockJSON turns a decoded clock path into the endpoint answering its links,
// reporting false for a source that is not one
func clockJSON(rawURL, clock string) (string, bool) {
	prefix := clock + clockPath + "?"
	if !strings.HasPrefix(rawURL, prefix) {
		return "", false
	}
	return clock + clockPath + ".json?" + rawURL[len(prefix):], true
}

// expandClocks replaces the site's own encodes with the streams they resolve
// to, leaving every other source as it arrived
// a clock source names a path rather than a url, so without this the storage
// the site plays from is listed as an embed and the provider never plays at all
// one that fails keeps its entry, which reads as a single unplayable source
// rather than as the provider carrying nothing
func (b *Backend) expandClocks(ctx context.Context, res *miruro.Result) {
	if len(res.Streams) == 0 {
		return
	}
	out := make([][]miruro.Stream, len(res.Streams))
	var wg sync.WaitGroup
	for i, s := range res.Streams {
		jsonURL, ok := clockJSON(s.URL, b.clock())
		if !ok {
			out[i] = []miruro.Stream{s}
			continue
		}
		wg.Go(func() {
			expanded, err := b.links(ctx, jsonURL, s)
			if err != nil {
				log.Debug("allanime clock source did not resolve", "server", s.Server, "err", err)
				out[i] = []miruro.Stream{s}
				return
			}
			out[i] = expanded
		})
	}
	wg.Wait()
	res.Streams = slices.Concat(out...)
}

// clock is the origin the encoded source paths address
func (b *Backend) clock() string { return cmp.Or(b.Clock, defaultClock) }

// links asks one clock endpoint for the streams behind it
func (b *Backend) links(ctx context.Context, jsonURL string, from miruro.Stream) ([]miruro.Stream, error) {
	body, err := b.get(ctx, jsonURL)
	if err != nil {
		return nil, err
	}
	var answer struct {
		Links []link `json:"links"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("%w: allanime clock: %v", miruro.ErrUpstream, err)
	}
	if len(answer.Links) > maxLinks {
		return nil, fmt.Errorf("%w: allanime clock carried %d links", miruro.ErrUpstream, len(answer.Links))
	}
	var out []miruro.Stream
	for _, l := range answer.Links {
		if s, ok := clockStream(l, from); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: allanime clock carried no playable link", miruro.ErrUpstream)
	}
	return out, nil
}

// clockStream maps one answered link onto the source that named it, keeping the
// server name and referer so the stream still reads as that source
// the container is taken from what the answer declares and from the path when
// it declares nothing, and a link that names neither stays an embed rather than
// being handed to a player as a guess
func clockStream(l link, from miruro.Stream) (miruro.Stream, bool) {
	u, err := url.Parse(l.Link)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return miruro.Stream{}, false
	}
	s := from
	s.URL = u.String()
	switch {
	case l.HLS, strings.Contains(u.Path, ".m3u8"):
		s.Kind = miruro.HLS
	case l.Mp4, strings.Contains(u.Path, ".mp4"):
		s.Kind = miruro.MP4
	}
	// the endpoint labels a master "Hls" and a direct file by height, and only a
	// height means anything to the quality pick
	if strings.HasSuffix(l.Resolution, "p") {
		s.Quality = l.Resolution
	}
	return s, true
}

// decode reverses the substitution the api applies to a source path, two hex
// digits per character
// a pair the table does not name is kept as written, the way the site's own
// decoder behaves
func decode(s string) string {
	var b strings.Builder
	for i := 0; i+1 < len(s); i += 2 {
		if c, ok := substitution[s[i:i+2]]; ok {
			b.WriteByte(c)
		} else {
			b.WriteString(s[i : i+2])
		}
	}
	return b.String()
}

// substitution is the api's table, hex pair to character
var substitution = map[string]byte{
	"79": 'A', "7a": 'B', "7b": 'C', "7c": 'D', "7d": 'E', "7e": 'F', "7f": 'G',
	"70": 'H', "71": 'I', "72": 'J', "73": 'K', "74": 'L', "75": 'M', "76": 'N', "77": 'O',
	"68": 'P', "69": 'Q', "6a": 'R', "6b": 'S', "6c": 'T', "6d": 'U', "6e": 'V', "6f": 'W',
	"60": 'X', "61": 'Y', "62": 'Z',
	"59": 'a', "5a": 'b', "5b": 'c', "5c": 'd', "5d": 'e', "5e": 'f', "5f": 'g',
	"50": 'h', "51": 'i', "52": 'j', "53": 'k', "54": 'l', "55": 'm', "56": 'n', "57": 'o',
	"48": 'p', "49": 'q', "4a": 'r', "4b": 's', "4c": 't', "4d": 'u', "4e": 'v', "4f": 'w',
	"40": 'x', "41": 'y', "42": 'z',
	"08": '0', "09": '1', "0a": '2', "0b": '3', "0c": '4', "0d": '5', "0e": '6', "0f": '7',
	"00": '8', "01": '9',
	"15": '-', "16": '.', "67": '_', "46": '~',
	"02": ':', "17": '/', "07": '?', "1b": '#',
	"63": '[', "65": ']', "78": '@',
	"19": '!', "1c": '$', "1e": '&',
	"10": '(', "11": ')', "12": '*', "13": '+', "14": ',',
	"03": ';', "05": '=', "1d": '%',
}

// parseNumber reads an episode string, which the api keeps as text so a half
// episode can read 12.5
func parseNumber(s string) (float64, error) {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("episode %q", s)
	}
	return n, nil
}
