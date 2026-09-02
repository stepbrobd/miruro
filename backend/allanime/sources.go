package allanime

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

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
// the site's own encode arrives as a player source, and everything else is an
// embedded page or an encoded path to a source host, which nothing here plays
// and which is listed as an embed so the provider reads as resolved rather
// than empty
func streams(plain []byte, cat miruro.Category, referer string) (*miruro.Result, error) {
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
		st, ok := stream(s, referer)
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
func stream(s source, referer string) (miruro.Stream, bool) {
	raw := s.URL
	if strings.HasPrefix(raw, "--") {
		raw = clockHost + decode(raw[2:])
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

// clockHost serves the encoded source paths the api hands out
// its endpoint stalled on every real id on 2026-09-02, so the paths are
// decoded for the record and listed as embeds rather than fetched
const clockHost = "https://allanime.day"

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
