// Package allanime is the AllAnime backend, the site miruro's ally provider
// fronts, reached directly so its streams stay up when miruro's are down.
// The api serves search and episode lists to anyone, and resolves an episode
// only for a client that proves it runs the site's current bundle: a session
// key built from constants in that bundle and a bootstrap the api signs, and a
// sealed token on every episode request.
package allanime

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/log"

	"ysun.co/miruro"
)

// Code is the provider code the backend lists its streams under
const Code = "allanime"

const (
	defaultSite = "https://mkissa.to"
	defaultAPI  = "https://api.mkissa.net"
	// lane is the request class the api files episode resolution under
	lane = "k7"
	// group is the key group the site's own host belongs to
	group = "mkissa"
	// maxBody caps an api answer and a bundle file alike
	maxBody = 4 << 20
	// maxChunks bounds how many bundle chunks the constant search reads
	maxChunks = 64
	// searchPage is how many shows one title search asks for
	searchPage = 40
)

// Backend resolves AllAnime shows keyed by AniList id
type Backend struct {
	HTTP *http.Client
	// Site is the frontend origin the api binds requests to
	Site string
	// API is the graphql origin
	API string

	mu      sync.Mutex
	build   *build
	session *session
}

// session is one epoch's key, valid until the api rotates it
type session struct {
	key      []byte
	epoch    int64
	switchAt time.Time
}

func New() *Backend {
	// the cloned default transport keeps HTTP/2 via ALPN
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &Backend{
		HTTP: &http.Client{Transport: tr, Timeout: 2 * time.Minute},
		Site: defaultSite,
		API:  defaultAPI,
	}
}

func (b *Backend) Name() string { return Code }

// Capabilities declares the one rendition the site serves
// its own encode of a sub episode carries no subtitle track, so the subtitles
// are in the picture, and there is no separate soft rendition to ask for
func (b *Backend) Capabilities(context.Context) (miruro.Capabilities, error) {
	return miruro.Capabilities{Code: {Hard: true}}, nil
}

// headers is the browser profile the api and the cdn expect
func (b *Backend) headers(h http.Header) {
	h.Set("User-Agent", miruro.UserAgent)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Accept-Language", "en-US,en;q=0.5")
	h.Set("Referer", b.Site+"/")
	h.Set("Origin", b.Site)
}

// get reads one whole body, refusing one over maxBody
func (b *Backend) get(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	b.headers(req.Header)
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", miruro.ErrUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s: status %d", miruro.ErrUpstream, rawURL, resp.StatusCode)
	}
	return capped(resp.Body)
}

func capped(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", miruro.ErrUpstream, err)
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("%w: body exceeds %d bytes", miruro.ErrUpstream, maxBody)
	}
	return body, nil
}

var (
	appRe   = regexp.MustCompile(`https?://[^"'\s]+/entry/app\.[A-Za-z0-9_.-]+\.js`)
	chunkRe = regexp.MustCompile(`"\.\./chunks/[A-Za-z0-9_.-]+\.js"`)
)

// fetchBuild reads the current build constants off the live site
// the page names the entry bundle, the entry names its chunks, and one of the
// chunks carries the constants
func (b *Backend) fetchBuild(ctx context.Context) (*build, error) {
	page, err := b.get(ctx, b.Site+"/")
	if err != nil {
		return nil, err
	}
	appURL := appRe.Find(page)
	if appURL == nil {
		return nil, fmt.Errorf("%w: page names no entry bundle", errBundle)
	}
	root, _, ok := strings.Cut(string(appURL), "/entry/")
	if !ok {
		return nil, fmt.Errorf("%w: entry bundle url %s", errBundle, appURL)
	}
	app, err := b.get(ctx, string(appURL))
	if err != nil {
		return nil, err
	}
	chunks := chunkRe.FindAll(app, maxChunks)
	if len(chunks) == 0 {
		return nil, fmt.Errorf("%w: entry bundle names no chunks", errBundle)
	}
	for _, c := range chunks {
		name := strings.TrimPrefix(strings.Trim(string(c), `"`), "../")
		js, err := b.get(ctx, root+"/"+name)
		if err != nil {
			return nil, err
		}
		if !bytes.Contains(js, []byte("saltMul:")) {
			continue
		}
		return parseBuild(string(js))
	}
	return nil, fmt.Errorf("%w: no chunk carries the constants", errBundle)
}

// bootstrap asks the api for the server half of the session key
// the request is signed with the mask and the epoch, and the api answers the
// previous epoch inside its grace period, so that one is tried first the way
// the site does
func (b *Backend) bootstrap(ctx context.Context, bd *build) (*session, error) {
	host := b.Site
	if u, err := url.Parse(b.Site); err == nil {
		host = strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	}
	now := time.Now()
	current := now.UnixMilli() / bd.epoch.Milliseconds()
	epochs := []int64{current}
	if now.UnixMilli()-current*bd.epoch.Milliseconds() < bd.grace.Milliseconds() && current > 0 {
		epochs = []int64{current - 1, current}
	}

	var last error
	for _, epoch := range epochs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			b.API+"/client-crypto/v1/bootstrap?buildId="+url.QueryEscape(bd.ID)+"&k="+lane, nil)
		if err != nil {
			return nil, err
		}
		b.headers(req.Header)
		req.Header.Set("x-build-id", bd.ID)
		req.Header.Set("x-aa-boot", bd.boot(host, group, lane, epoch))
		resp, err := b.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", miruro.ErrUpstream, err)
		}
		body, err := capped(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			last = fmt.Errorf("%w: bootstrap for epoch %d: status %d", errCrypto, epoch, resp.StatusCode)
			continue
		}
		var raw struct {
			PartB    string `json:"partB"`
			Epoch    int64  `json:"epoch"`
			SwitchAt int64  `json:"switchAt"`
			K        string `json:"k"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || raw.PartB == "" {
			last = fmt.Errorf("%w: bootstrap answered no key", errCrypto)
			continue
		}
		if raw.K != "" && raw.K != lane {
			return nil, fmt.Errorf("%w: bootstrap answered lane %q", errCrypto, raw.K)
		}
		partB, err := base64.StdEncoding.DecodeString(raw.PartB)
		if err != nil {
			return nil, fmt.Errorf("%w: bootstrap key is not base64", errCrypto)
		}
		key, err := bd.sessionKey(partB)
		if err != nil {
			return nil, err
		}
		return &session{key: key, epoch: raw.Epoch, switchAt: time.UnixMilli(raw.SwitchAt)}, nil
	}
	return nil, last
}

// attest returns the build and a live session, fetching either when it is
// missing or when the api said the last one had gone stale
func (b *Backend) attest(ctx context.Context, stale bool) (*build, *session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if stale {
		b.build, b.session = nil, nil
	}
	if b.build == nil {
		bd, err := b.fetchBuild(ctx)
		if err != nil {
			return nil, nil, err
		}
		b.build, b.session = bd, nil
	}
	if b.session == nil || !time.Now().Before(b.session.switchAt) {
		s, err := b.bootstrap(ctx, b.build)
		if err != nil {
			return nil, nil, err
		}
		b.session = s
	}
	return b.build, b.session, nil
}

// reply is one graphql answer
type reply struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message    string `json:"message"`
		Extensions struct {
			Code string `json:"code"`
		} `json:"extensions"`
	} `json:"errors"`
}

// query runs one graphql request
// a cloudflare challenge is what the api answers a client it will not talk
// to at all, and it stays fatal the way a miruro block does
func (b *Backend) query(ctx context.Context, body map[string]any, buildID string) (*reply, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.API+"/api", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	b.headers(req.Header)
	req.Header.Set("Content-Type", "application/json")
	if buildID != "" {
		req.Header.Set("x-build-id", buildID)
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", miruro.ErrUpstream, err)
	}
	defer resp.Body.Close()
	raw, err := capped(resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	switch {
	case resp.Header.Get("cf-mitigated") != "":
		return nil, fmt.Errorf("%w: allanime", miruro.ErrBlocked)
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("%w: allanime status %d", miruro.ErrUpstream, resp.StatusCode)
	}
	var r reply
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%w: allanime answered %s", miruro.ErrUpstream, resp.Header.Get("content-type"))
	}
	return &r, nil
}

// crypto reports whether an answer refused the attestation, which a fresh
// build and session can correct
func (r *reply) crypto() bool {
	for _, e := range r.Errors {
		if strings.HasPrefix(e.Extensions.Code, "AA_CRYPTO_") {
			return true
		}
	}
	return false
}

// err maps an answer's first error
// NEED_CAPTCHA is the api asking this client to solve a Turnstile challenge
// before it resolves anything else, which a cli cannot do, so it is the block
// the fallback walks around rather than an outage worth another request
func (r *reply) err() error {
	if len(r.Errors) == 0 {
		return nil
	}
	if r.Errors[0].Message == "NEED_CAPTCHA" {
		return fmt.Errorf("%w: allanime asks for a captcha", miruro.ErrBlocked)
	}
	return fmt.Errorf("%w: allanime: %s", miruro.ErrUpstream, r.Errors[0].Message)
}

// show is one search hit
type show struct {
	ID        string `json:"_id"`
	Name      string `json:"name"`
	AniListID string `json:"aniListId"`
	Available struct {
		Sub []string `json:"sub"`
		Dub []string `json:"dub"`
	} `json:"availableEpisodesDetail"`
}

const searchQuery = `query($search: SearchInput, $limit: Int) {
  shows(search: $search, limit: $limit, translationType: sub, countryOrigin: ALL) {
    edges { _id name aniListId availableEpisodesDetail }
  }
}`

func (b *Backend) search(ctx context.Context, title string) ([]show, error) {
	r, err := b.query(ctx, map[string]any{
		"query": searchQuery,
		"variables": map[string]any{
			"search": map[string]any{"query": title, "allowAdult": false, "allowUnknown": false},
			"limit":  searchPage,
		},
	}, "")
	if err != nil {
		return nil, err
	}
	if err := r.err(); err != nil {
		return nil, err
	}
	var data struct {
		Shows struct {
			Edges []show `json:"edges"`
		} `json:"shows"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		return nil, fmt.Errorf("%w: allanime search: %v", miruro.ErrUpstream, err)
	}
	return data.Shows.Edges, nil
}

// Episodes finds the show carrying the title's AniList id and lists its
// episodes
// the api searches by name only, so each title the media carries is searched
// and the hit is the one naming the id, which is what keeps a season from
// resolving to its sequel
// a title the site does not carry is an empty catalog rather than an error
func (b *Backend) Episodes(ctx context.Context, m miruro.Media) (*miruro.Catalog, error) {
	cat := &miruro.Catalog{Providers: map[string]miruro.Provider{}}
	want := fmt.Sprint(m.ID)
	var titles []string
	for _, t := range []string{m.English, m.Romaji} {
		if t != "" && (len(titles) == 0 || titles[0] != t) {
			titles = append(titles, t)
		}
	}
	for _, title := range titles {
		shows, err := b.search(ctx, title)
		if err != nil {
			return nil, err
		}
		for _, s := range shows {
			if s.AniListID != want || s.ID == "" {
				continue
			}
			cat.Providers[Code] = miruro.Provider{
				Code:    Code,
				Backend: b,
				Sub:     episodes(s.ID, s.Available.Sub),
				Dub:     episodes(s.ID, s.Available.Dub),
			}
			return cat, nil
		}
	}
	return cat, nil
}

// episodes turns the api's episode strings into records
// the id carries the show, since the resolver needs both and the episode
// string alone names nothing
func episodes(showID string, strs []string) []miruro.Episode {
	var out []miruro.Episode
	for _, s := range strs {
		n, err := parseNumber(s)
		if err != nil {
			continue
		}
		out = append(out, miruro.Episode{ID: showID + "/" + s, Number: n})
	}
	slices.SortFunc(out, func(a, b miruro.Episode) int { return cmp.Compare(a.Number, b.Number) })
	return out
}

const episodeQuery = `query($showId: String!, $translationType: VaildTranslationTypeEnumType!, $episodeString: String!) {
  episode(showId: $showId, translationType: $translationType, episodeString: $episodeString) {
    episodeString
    sourceUrls
    show { _id name countryOfOrigin }
    episodeInfo { vidInforssub vidInforsdub }
  }
}`

// queryHash is the persisted query id the api binds the token to, the hash of
// the query text the way the site's client computes it
var queryHash = func() string {
	sum := sha256.Sum256([]byte(episodeQuery))
	return hex.EncodeToString(sum[:])
}()

// Sources resolves an episode to the streams the site can serve
func (b *Backend) Sources(ctx context.Context, episodeID, provider string, cat miruro.Category) (*miruro.Result, error) {
	if provider != Code {
		return nil, fmt.Errorf("allanime does not serve provider %s", provider)
	}
	showID, episode, ok := strings.Cut(episodeID, "/")
	if !ok || showID == "" || episode == "" {
		return nil, fmt.Errorf("allanime episode id %q names no show", episodeID)
	}
	translation, ok := translations[cat]
	if !ok {
		return nil, fmt.Errorf("%w: allanime carries no %s rendition", miruro.ErrUpstream, cat)
	}

	plain, err := b.resolve(ctx, showID, episode, translation)
	if err != nil {
		return nil, err
	}
	res, err := streams(plain, cat, b.Site)
	if err != nil {
		return nil, err
	}
	for _, s := range res.Streams {
		log.Debug("allanime source", "server", s.Server, "kind", s.Kind, "quality", s.Quality)
	}
	return res, nil
}

// translations maps a category to the api's translation type
// ssub is absent because the site has no separate soft-subtitled rendition
var translations = map[miruro.Category]string{
	miruro.Sub: "sub",
	miruro.Dub: "dub",
}

// resolve runs the attested episode query and returns the opened answer
// an attestation the api refuses is retried once on a fresh build and session,
// which is what a deploy or an epoch boundary between two episodes needs
func (b *Backend) resolve(ctx context.Context, showID, episode, translation string) ([]byte, error) {
	stale := false
	for attempt := range 2 {
		bd, s, err := b.attest(ctx, stale)
		if err != nil {
			return nil, err
		}
		tok, err := token(s.key, s.epoch, bd.ID, queryHash, lane, time.Now())
		if err != nil {
			return nil, err
		}
		r, err := b.query(ctx, map[string]any{
			"query": episodeQuery,
			"variables": map[string]any{
				"showId": showID, "translationType": translation, "episodeString": episode,
			},
			"extensions": map[string]any{
				"persistedQuery": map[string]any{"version": 1, "sha256Hash": queryHash},
				"k":              lane,
				"aaReq":          tok,
			},
		}, bd.ID)
		if err != nil {
			return nil, err
		}
		if r.crypto() {
			if attempt == 0 {
				log.Debug("allanime refused the attestation, refreshing", "err", r.Errors[0].Extensions.Code)
				stale = true
				continue
			}
			return nil, fmt.Errorf("%w: %s", errCrypto, r.Errors[0].Extensions.Code)
		}
		if err := r.err(); err != nil {
			return nil, err
		}
		var sealed struct {
			ToBeParsed string `json:"tobeparsed"`
		}
		if err := json.Unmarshal(r.Data, &sealed); err != nil || sealed.ToBeParsed == "" {
			return nil, fmt.Errorf("%w: allanime answered no sealed payload", miruro.ErrUpstream)
		}
		return open(s.key, sealed.ToBeParsed)
	}
	return nil, errCrypto
}
