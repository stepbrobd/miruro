package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"ysun.co/miruro/internal/miruro"
	"ysun.co/miruro/internal/play"
	"ysun.co/miruro/internal/upstream"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor [query]",
	Short: "Check the setup, and with a query every provider serving the title",
	Long: `Check the config, the player, ffmpeg and each api origin.

With a query, also look the title up, list the providers serving its first
episode, and walk each one through the stream proxy the way a player starts
it, naming the host behind every answer.`,
	RunE: runDoctor,
}

func init() {
	root.AddCommand(doctorCmd)
}

func runDoctor(cmd *cobra.Command, args []string) error {
	return (&doctor{out: os.Stdout}).run(cmd.Context(), loadConfig(), args)
}

// run makes every check, the title's only for a query, and fails when any did
func (d *doctor) run(ctx context.Context, cfg config, args []string) error {
	d.config()
	d.player(cfg)
	d.tools()

	client := miruro.New()
	if len(cfg.Mirrors) > 0 {
		client.Bases = cfg.Mirrors
	}
	d.mirrors(ctx, client.Bases)
	if len(args) > 0 {
		d.title(ctx, client, cfg, strings.Join(args, " "))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.failed > 0 {
		return fmt.Errorf("%s failed", plural(d.failed, "check", "checks"))
	}
	return nil
}

// doctor prints one row per check as it finishes, since walking every provider
// takes long enough that a table printed at the end would look hung
type doctor struct {
	out    io.Writer
	failed int
}

// row prints one check, and a failed one counts against the run
func (d *doctor) row(name string, err error, detail string) {
	verdict := "ok"
	if err != nil {
		verdict = "fail"
		d.failed++
		detail = strings.TrimSpace(detail + ", " + err.Error())
		detail = strings.TrimPrefix(detail, ", ")
	}
	fmt.Fprintf(d.out, "%-14s %-4s %s\n", name, verdict, detail)
}

// note prints a check that found something missing and nothing broken
func (d *doctor) note(name, detail string) {
	fmt.Fprintf(d.out, "%-14s %-4s %s\n", name, "warn", detail)
}

// config reads the file the way config validate does, by its own values rather
// than the loaded ones, so an environment override cannot hide a broken key
func (d *doctor) config() {
	path, err := configPath()
	if err != nil {
		d.row("config", err, "")
		return
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		d.row("config", nil, "no file at "+path+", every setting a default")
		return
	case err != nil:
		d.row("config", err, path)
		return
	}
	var c config
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		d.row("config", err, path)
		return
	}
	problems := check(md, c)
	if len(problems) == 0 {
		d.row("config", nil, path)
	}
	for _, p := range problems {
		d.row("config", errors.New(p), "")
	}
}

// player finds what a run would play with
func (d *doctor) player(cfg config) {
	p, err := play.Detect(play.Kind(cfg.Player))
	if err != nil {
		d.row("player", err, "")
		return
	}
	d.row("player", nil, fmt.Sprintf("%s at %s", p.Kind, p.Bin))
}

// tools looks for what downloads use and playback does without, so a missing
// one is a warning rather than a failure
func (d *doctor) tools() {
	for _, t := range []struct{ bin, without string }{
		{"ffmpeg", "hls downloads fail without it"},
		{"ffprobe", "downloads go unchecked for sound without it"},
	} {
		if path, err := exec.LookPath(t.bin); err == nil {
			d.row(t.bin, nil, path)
		} else {
			d.note(t.bin, "not found, "+t.without)
		}
	}
}

// mirrors asks each api origin on its own, since a run walks past a dead one
// without a word and only asking each says which are dead
func (d *doctor) mirrors(ctx context.Context, bases []string) {
	for _, base := range bases {
		c := miruro.New()
		c.Bases = []string{base}
		pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		start := time.Now()
		err := c.Ping(pctx)
		cancel()
		host := base
		if u, perr := url.Parse(base); perr == nil && u.Host != "" {
			host = u.Host
		}
		d.row("api", err, fmt.Sprintf("%s in %s", host, time.Since(start).Round(time.Millisecond)))
	}
}

// title looks the query up and walks every provider serving its first episode
// the first hit is taken without asking, and the row names it, so a query
// matching the wrong title is plain to see
func (d *doctor) title(ctx context.Context, client *miruro.Client, cfg config, query string) {
	hits, _, err := client.Search(ctx, query, "")
	if err == nil && len(hits) == 0 {
		err = fmt.Errorf("nothing matches %q", query)
	}
	if err != nil {
		d.row("title", err, "")
		return
	}
	m := hits[0]
	d.row("title", nil, fmt.Sprintf("%s, anilist %d, first of %s", m.Title(), m.ID, plural(len(hits), "hit", "hits")))

	backends := enabled(all(client), cfg.Backends)
	cat, failed := backends.Episodes(ctx, m)
	for _, f := range failed {
		d.row("catalog", f.Err, f.Backend)
	}
	category := upstream.Sub
	if cfg.Dub {
		category = upstream.Dub
	}
	d.row("catalog", nil, fmt.Sprintf("%d sub and %d dub episodes", len(cat.Numbers(upstream.Sub)), len(cat.Numbers(upstream.Dub))))
	st := &runState{
		hc: upstream.Public(), backends: backends, cat: cat, anilistID: m.ID,
		category: category, cfg: cfg, fallback: true,
	}
	d.providers(ctx, st)
}

// providers lists the first episode and walks every provider row a picker would
// offer for it, each rendition of a provider its own row
func (d *doctor) providers(ctx context.Context, st *runState) {
	numbers := st.cat.Numbers(st.category)
	if len(numbers) == 0 {
		d.row("episode", fmt.Errorf("no %s episodes", st.category), "")
		return
	}
	ep := numbers[0]
	name := "episode " + num(ep)
	l, err := st.listing(ctx, ep)
	if err != nil {
		d.row(name, err, "")
		return
	}
	avail, err := candidates(l, ep, st.category)
	if err != nil {
		d.row(name, err, "")
		return
	}
	rows := orderPinned(offers(avail, l.Caps, st.category, Pin{}), Pin{})
	var codes []string
	for _, o := range rows {
		if !slices.Contains(codes, o.Code) {
			codes = append(codes, o.Code)
		}
	}
	d.row(name, nil, strings.Join(codes, " ")+" serve it")

	px, err := play.StartProxy(ctx, st.hc)
	if err != nil {
		d.row("proxy", err, "")
		return
	}
	defer px.Close()
	for _, o := range rows {
		if ctx.Err() != nil {
			return
		}
		d.provider(ctx, st, l, px, ep, o)
	}
}

// providerProbe bounds one provider's resolve and walk, so a stalled CDN costs
// one row rather than the whole check
const providerProbe = 45 * time.Second

// provider resolves one row the way a run would and walks its first ranked
// stream
func (d *doctor) provider(ctx context.Context, st *runState, l *upstream.Listing, px *play.Proxy, ep float64, o offer) {
	src := o.source(st.category)
	name := o.Code + " " + string(src.Category)
	e := find(l.Providers[o.Code].Episodes(src.Category), ep)
	if e == nil {
		d.row(name, errors.New("lists no such episode"), "")
		return
	}
	pctx, cancel := context.WithTimeout(ctx, providerProbe)
	defer cancel()
	res, err := l.Sources(pctx, e.ID, o.Code, src.Category)
	if err != nil {
		d.row(name, err, "")
		return
	}
	ranked := upstream.Rank(pctx, st.hc, res, st.cfg.Quality)
	if len(ranked) == 0 {
		d.row(name, fmt.Errorf("none of %s plays", plural(len(res.Streams), "stream", "streams")), "")
		return
	}
	s := ranked[0]
	walked, err := walkStream(pctx, px, s)
	d.row(name, err, fmt.Sprintf("%s %s %s of %d, %s", server(s), s.Kind, or(s.Quality, "unlabeled"), len(ranked), walked))
}

// segmentProbe is how many segments of a stream the doctor fetches, enough to
// see a CDN refuse them without pulling an episode
const segmentProbe = 3

// probeRange is how much of an mp4 the doctor asks for
const probeRange = 64 << 10

// walkStream fetches a stream through the proxy the way a player starts it,
// the master, the media playlist and the first few segments, and says what
// each answered and which host was behind it
// it fails when the proxy relayed no media body, which is a stream a player
// never starts
func walkStream(ctx context.Context, px *play.Proxy, s upstream.Stream) (string, error) {
	tl := px.Tally()
	// the proxy listens on loopback, which the run's guarded client refuses
	hc := &http.Client{}
	host := func(proxied string) string {
		raw, err := px.Upstream(proxied)
		if err != nil {
			return "?"
		}
		if u, err := url.Parse(raw); err == nil {
			return u.Host
		}
		return "?"
	}
	get := func(u string, limit int64) ([]byte, int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, 0, err
		}
		if limit > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", limit-1))
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, 0, bare(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		return body, resp.StatusCode, bare(err)
	}
	var steps []string
	done := func(err error) (string, error) {
		steps = append(steps, fmt.Sprintf("served %d refused %d", tl.Served(), tl.Refused()))
		if err == nil && tl.Served() == 0 {
			err = errors.New("nothing relayed")
		}
		return strings.Join(steps, ", "), err
	}

	u := tl.Stream(s).URL
	if s.Kind == upstream.MP4 {
		_, code, err := get(u, probeRange)
		steps = append(steps, fmt.Sprintf("body %d from %s", code, host(u)))
		return done(err)
	}
	body, code, err := get(u, 0)
	steps = append(steps, fmt.Sprintf("master %d from %s", code, host(u)))
	if err != nil || code != http.StatusOK {
		return done(err)
	}
	if variant := bestURI(body); variant != "" {
		u = variant
		if body, code, err = get(u, 0); err != nil || code != http.StatusOK {
			steps = append(steps, fmt.Sprintf("media %d", code))
			return done(err)
		}
		steps = append(steps, fmt.Sprintf("media %d", code))
	}
	var codes, hosts []string
	for i, seg := range uris(body, segmentProbe) {
		if h := host(seg); !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
		_, code, err := get(seg, 0)
		if err != nil {
			steps = append(steps, fmt.Sprintf("segments %s from %s", strings.Join(append(codes, "none"), " "), strings.Join(hosts, " ")))
			return done(fmt.Errorf("segment %d: %w", i, err))
		}
		codes = append(codes, strconv.Itoa(code))
	}
	steps = append(steps, fmt.Sprintf("segments %s from %s", strings.Join(codes, " "), strings.Join(hosts, " ")))
	return done(nil)
}

// bare drops the address a failed request names, which is a proxy payload no
// reader can use, and keeps why it failed
func bare(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// bandwidth reads a variant's advertised rate, matched whole so an
// AVERAGE-BANDWIDTH listed first is not taken for it
var bandwidth = regexp.MustCompile(`[:,]BANDWIDTH=(\d+)`)

// bestURI is the highest bandwidth variant of a master, the one a player opens
// first, empty for a playlist that is not a master
func bestURI(body []byte) string {
	best, bestRate, rate := "", int64(-1), int64(-1)
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	master := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
			master, rate = true, -1
			if m := bandwidth.FindStringSubmatch(line); m != nil {
				rate, _ = strconv.ParseInt(m[1], 10, 64)
			}
		case line == "" || strings.HasPrefix(line, "#"):
		case master:
			if best == "" || rate > bestRate {
				best, bestRate = line, rate
			}
			rate = -1
		}
	}
	return best
}

// uris is the first n entries a media playlist names
func uris(body []byte, n int) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() && len(out) < n {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}
