package allanime

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// errBundle marks a site bundle whose shape this package cannot read
// the constants below are minted by the site's obfuscator on every deploy, so
// a shape change is the expected way for this backend to break, and the
// failure names it rather than signing requests with a guess
var errBundle = errors.New("allanime bundle shape not recognised")

// build is what the site's bundle pins a client to
// the mask, the bootstrap signature, and the request token all derive from
// these values, and every one of them rotates when the site deploys
type build struct {
	ID string
	// frags are the four eight-byte fragments the mask is assembled from
	frags [][]byte
	// the salts spread the build id and the fragment position over the mask
	saltMul, saltAdd, fragMul, fragAdd int
	// prefix leads the build id in the first bootstrap signature round
	prefix string
	// join and parts describe the second round's message
	join  string
	parts []string
	// epoch is the key rotation period and grace how long the previous
	// epoch stays accepted past a boundary
	epoch, grace time.Duration
}

const (
	fragCount = 4
	fragLen   = 8
	// maxTable bounds the obfuscator string table the walk indexes into
	maxTable = 1 << 14
	// maxDepth bounds expression nesting, so a hostile bundle cannot recurse
	// the evaluator into the stack limit
	maxDepth = 64
)

// parseBuild reads the build constants out of the bundle chunk that carries
// them
// the chunk is javascript-obfuscator output, where every string literal lives
// in one rotated table and each use is an indexed call through a chain of
// offset wrappers, so the constants are recovered by finding the object that
// names the salts, walking back to the declaration statement it belongs to,
// and evaluating each declaration against the rotated table
func parseBuild(js string) (*build, error) {
	before, _, ok := strings.Cut(js, "saltMul:")
	if !ok {
		return nil, fmt.Errorf("%w: no salt config", errBundle)
	}
	open := strings.LastIndex(before, "{")
	start := strings.LastIndex(js[:open], "const ")
	if open < 0 || start < 0 {
		return nil, fmt.Errorf("%w: salt config outside a declaration", errBundle)
	}
	end, err := matching(js, open)
	if err != nil {
		return nil, err
	}
	stmt := js[start+len("const ") : end+1]

	ev := &evaluator{js: js, tables: map[string]*table{}}
	b := &build{}
	var numbers []float64
	for _, decl := range splitTop(stmt, ',') {
		_, expr, ok := strings.Cut(decl, "=")
		if !ok {
			return nil, fmt.Errorf("%w: declaration %q", errBundle, decl)
		}
		v, err := ev.eval(expr, js, nil)
		if err != nil {
			return nil, err
		}
		switch v := v.(type) {
		case string:
			b.ID = v
		case float64:
			numbers = append(numbers, v)
		case []value:
			if b.frags, err = fragments(v); err != nil {
				return nil, err
			}
		case map[string]value:
			if err := b.config(v); err != nil {
				return nil, err
			}
		}
	}
	if len(numbers) == 2 {
		b.epoch = time.Duration(math.Max(numbers[0], numbers[1])) * time.Millisecond
		b.grace = time.Duration(math.Min(numbers[0], numbers[1])) * time.Millisecond
	}
	return b, b.check()
}

func (b *build) check() error {
	switch {
	case b.ID == "":
		return fmt.Errorf("%w: no build id", errBundle)
	case len(b.frags) != fragCount:
		return fmt.Errorf("%w: %d mask fragments", errBundle, len(b.frags))
	case b.prefix == "" || b.join == "" || len(b.parts) != 5:
		return fmt.Errorf("%w: bootstrap message shape", errBundle)
	case b.epoch <= 0 || b.grace <= 0 || b.grace >= b.epoch:
		return fmt.Errorf("%w: epoch %s grace %s", errBundle, b.epoch, b.grace)
	}
	return nil
}

func fragments(v []value) ([][]byte, error) {
	out := make([][]byte, 0, len(v))
	for _, f := range v {
		s, ok := f.(string)
		if !ok {
			return nil, fmt.Errorf("%w: fragment is not a string", errBundle)
		}
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(raw) != fragLen {
			return nil, fmt.Errorf("%w: fragment %q", errBundle, s)
		}
		out = append(out, raw)
	}
	return out, nil
}

func (b *build) config(obj map[string]value) error {
	num := func(key string) (int, error) {
		f, ok := obj[key].(float64)
		if !ok {
			return 0, fmt.Errorf("%w: config %s", errBundle, key)
		}
		return int(f) & 0xff, nil
	}
	str := func(key string) (string, error) {
		s, ok := obj[key].(string)
		if !ok {
			return "", fmt.Errorf("%w: config %s", errBundle, key)
		}
		return s, nil
	}
	var err error
	if b.saltMul, err = num("saltMul"); err != nil {
		return err
	}
	if b.saltAdd, err = num("saltAdd"); err != nil {
		return err
	}
	if b.fragMul, err = num("fragMul"); err != nil {
		return err
	}
	if b.fragAdd, err = num("fragAdd"); err != nil {
		return err
	}
	if b.prefix, err = str("bootPrefix"); err != nil {
		return err
	}
	if b.join, err = str("join"); err != nil {
		return err
	}
	parts, ok := obj["parts"].([]value)
	if !ok {
		return fmt.Errorf("%w: config parts", errBundle)
	}
	for _, p := range parts {
		s, ok := p.(string)
		if !ok {
			return fmt.Errorf("%w: config part is not a string", errBundle)
		}
		b.parts = append(b.parts, s)
	}
	return nil
}

// value is one evaluated javascript expression, a number, a string, an array,
// an object, or a boolean
type value any

// table is one obfuscator string array, rotated into the order the bundle's
// checksum loop settles on
type table struct {
	entries []string
}

// evaluator resolves the obfuscator's expression language, integer arithmetic
// over string table lookups reached through offset wrappers
type evaluator struct {
	js     string
	tables map[string]*table
}

var (
	funcRe  = regexp.MustCompile(`function ([A-Za-z_$][\w$]*)\(([^)]*)\)\{return ([^{}]*)\}`)
	baseRe  = regexp.MustCompile(`^([\w$]+)=([\w$]+)-\((.+)\),([\w$]+)\(\)\[([\w$]+)\]$`)
	tableRe = regexp.MustCompile(`function ([\w$]+)\(\)\{const ([\w$]+)=\[`)
	checkRe = regexp.MustCompile(`if\((.+?)===([\w$]+)\)break`)
)

// fn finds a one-expression function by name, preferring the region a call
// sits in, since an obfuscator scope reuses short names
func (ev *evaluator) fn(name, region string) (params []string, body string, ok bool) {
	for _, src := range []string{region, ev.js} {
		for _, m := range funcRe.FindAllStringSubmatch(src, -1) {
			if m[1] != name {
				continue
			}
			for p := range strings.SplitSeq(m[2], ",") {
				params = append(params, strings.TrimSpace(p))
			}
			return params, m[3], true
		}
	}
	return nil, "", false
}

// call evaluates a wrapper call, following the chain down to the string table
func (ev *evaluator) call(name string, args []value, region string, depth int) (value, error) {
	switch name {
	case "Number":
		if len(args) == 1 {
			if f, ok := args[0].(float64); ok {
				return f, nil
			}
		}
		return nil, fmt.Errorf("%w: Number()", errBundle)
	case "parseInt":
		if len(args) == 1 {
			if s, ok := args[0].(string); ok {
				return parseInt(s), nil
			}
		}
		return nil, fmt.Errorf("%w: parseInt()", errBundle)
	}
	params, body, ok := ev.fn(name, region)
	if !ok {
		return nil, fmt.Errorf("%w: function %s", errBundle, name)
	}
	env := map[string]value{}
	for i, p := range params {
		if i < len(args) {
			env[p] = args[i]
		}
	}
	if m := baseRe.FindStringSubmatch(body); m != nil {
		if m[1] != m[2] || m[1] != m[5] {
			return nil, fmt.Errorf("%w: table lookup %q", errBundle, body)
		}
		off, err := ev.eval(m[3], body, env)
		if err != nil {
			return nil, err
		}
		arg, ok := env[m[1]].(float64)
		shift, ok2 := off.(float64)
		if !ok || !ok2 {
			return nil, fmt.Errorf("%w: table index", errBundle)
		}
		t, err := ev.table(m[4])
		if err != nil {
			return nil, err
		}
		i := int(arg - shift)
		if i < 0 || i >= len(t.entries) {
			return nil, fmt.Errorf("%w: table index %d of %d", errBundle, i, len(t.entries))
		}
		return t.entries[i], nil
	}
	if depth > maxDepth {
		return nil, fmt.Errorf("%w: wrapper chain too deep", errBundle)
	}
	return ev.evalDepth(body, ev.js, env, depth+1)
}

// table parses and rotates the string array a base wrapper indexes
func (ev *evaluator) table(name string) (*table, error) {
	if t, ok := ev.tables[name]; ok {
		return t, nil
	}
	var entries []string
	found := false
	for _, m := range tableRe.FindAllStringSubmatchIndex(ev.js, -1) {
		if ev.js[m[2]:m[3]] != name {
			continue
		}
		var err error
		if entries, err = stringArray(ev.js[m[1]-1:]); err != nil {
			return nil, err
		}
		found = true
		break
	}
	if !found {
		return nil, fmt.Errorf("%w: string table %s", errBundle, name)
	}
	if len(entries) > maxTable {
		return nil, fmt.Errorf("%w: string table of %d entries", errBundle, len(entries))
	}
	t := &table{entries: entries}
	// the wrappers the checksum uses index this table, so it is registered
	// before the rotation runs
	ev.tables[name] = t
	if err := ev.rotate(name, t); err != nil {
		delete(ev.tables, name)
		return nil, err
	}
	return t, nil
}

// rotate replays the bundle's own loop, shifting the table one entry at a time
// until the checksum expression it carries matches the target
func (ev *evaluator) rotate(name string, t *table) error {
	callAt := strings.Index(ev.js, "})("+name+",")
	if callAt < 0 {
		return fmt.Errorf("%w: no rotation for %s", errBundle, name)
	}
	region := ev.js[:callAt]
	iife := strings.LastIndex(region, "(function(")
	if iife < 0 {
		return fmt.Errorf("%w: rotation for %s has no body", errBundle, name)
	}
	region = region[iife:]
	m := checkRe.FindStringSubmatch(region)
	if m == nil {
		return fmt.Errorf("%w: rotation for %s has no checksum", errBundle, name)
	}
	rest := ev.js[callAt+len("})("+name+","):]
	digits := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if digits <= 0 {
		return fmt.Errorf("%w: rotation for %s has no target", errBundle, name)
	}
	target, err := strconv.ParseFloat(rest[:digits], 64)
	if err != nil {
		return fmt.Errorf("%w: rotation target %q", errBundle, rest[:digits])
	}
	for range t.entries {
		v, err := ev.eval(m[1], region, map[string]value{m[2]: target})
		if f, ok := v.(float64); err == nil && ok && f == target {
			return nil
		}
		t.entries = append(t.entries[1:], t.entries[0])
	}
	return fmt.Errorf("%w: rotation for %s never settles", errBundle, name)
}

// parseInt follows javascript, reading the leading integer and nothing after
func parseInt(s string) float64 {
	s = strings.TrimSpace(s)
	i := 0
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		i++
	}
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	f, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

func (ev *evaluator) eval(expr, region string, env map[string]value) (value, error) {
	return ev.evalDepth(expr, region, env, 0)
}

func (ev *evaluator) evalDepth(expr, region string, env map[string]value, depth int) (value, error) {
	p := &parser{ev: ev, src: expr, region: region, env: env, depth: depth}
	v, err := p.expr()
	if err != nil {
		return nil, err
	}
	p.skip()
	if p.pos != len(p.src) {
		return nil, fmt.Errorf("%w: trailing %q in %q", errBundle, p.src[p.pos:], expr)
	}
	return v, nil
}

// parser is a recursive descent over the expression grammar the obfuscator
// emits, additive and multiplicative arithmetic, unary minus and not, calls,
// string and number literals, arrays, and objects
type parser struct {
	ev     *evaluator
	src    string
	pos    int
	region string
	env    map[string]value
	depth  int
}

func (p *parser) skip() {
	for p.pos < len(p.src) && p.src[p.pos] == ' ' {
		p.pos++
	}
}

func (p *parser) peek() byte {
	p.skip()
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *parser) expr() (value, error) {
	left, err := p.term()
	if err != nil {
		return nil, err
	}
	for {
		switch p.peek() {
		case '+':
			p.pos++
			right, err := p.term()
			if err != nil {
				return nil, err
			}
			if left, err = add(left, right); err != nil {
				return nil, err
			}
		case '-':
			p.pos++
			right, err := p.term()
			if err != nil {
				return nil, err
			}
			a, b, err := numbers(left, right)
			if err != nil {
				return nil, err
			}
			left = a - b
		default:
			return left, nil
		}
	}
}

func (p *parser) term() (value, error) {
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		op := p.peek()
		if op != '*' && op != '/' {
			return left, nil
		}
		p.pos++
		right, err := p.unary()
		if err != nil {
			return nil, err
		}
		a, b, err := numbers(left, right)
		if err != nil {
			return nil, err
		}
		if op == '*' {
			left = a * b
		} else {
			left = a / b
		}
	}
}

func (p *parser) unary() (value, error) {
	switch p.peek() {
	case '-':
		p.pos++
		v, err := p.unary()
		if err != nil {
			return nil, err
		}
		f, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("%w: negating a non-number", errBundle)
		}
		return -f, nil
	case '!':
		p.pos++
		v, err := p.unary()
		if err != nil {
			return nil, err
		}
		f, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("%w: not of a non-number", errBundle)
		}
		return f == 0, nil
	}
	return p.primary()
}

func (p *parser) primary() (value, error) {
	if p.depth > maxDepth {
		return nil, fmt.Errorf("%w: expression too deep", errBundle)
	}
	switch c := p.peek(); {
	case c == '(':
		p.pos++
		p.depth++
		v, err := p.expr()
		p.depth--
		if err != nil {
			return nil, err
		}
		if p.peek() != ')' {
			return nil, fmt.Errorf("%w: unclosed paren in %q", errBundle, p.src)
		}
		p.pos++
		return v, nil
	case c == '"' || c == '\'' || c == '`':
		s, n, err := stringLiteral(p.src[p.pos:])
		if err != nil {
			return nil, err
		}
		p.pos += n
		return s, nil
	case c == '[':
		return p.array()
	case c == '{':
		return p.object()
	case c >= '0' && c <= '9' || c == '.':
		return p.number()
	case c == '_' || c == '$' || unicode.IsLetter(rune(c)):
		return p.ident()
	}
	return nil, fmt.Errorf("%w: unexpected %q in %q", errBundle, p.src[p.pos:], p.src)
}

func (p *parser) number() (value, error) {
	start := p.pos
	for p.pos < len(p.src) && (p.src[p.pos] >= '0' && p.src[p.pos] <= '9' || p.src[p.pos] == '.') {
		p.pos++
	}
	f, err := strconv.ParseFloat(p.src[start:p.pos], 64)
	if err != nil {
		return nil, fmt.Errorf("%w: number %q", errBundle, p.src[start:p.pos])
	}
	return f, nil
}

func (p *parser) ident() (value, error) {
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '_' || c == '$' || c >= '0' && c <= '9' || unicode.IsLetter(rune(c)) {
			p.pos++
			continue
		}
		break
	}
	name := p.src[start:p.pos]
	if p.peek() != '(' {
		if v, ok := p.env[name]; ok {
			return v, nil
		}
		return nil, fmt.Errorf("%w: unbound %s", errBundle, name)
	}
	p.pos++
	var args []value
	for p.peek() != ')' {
		p.depth++
		v, err := p.expr()
		p.depth--
		if err != nil {
			return nil, err
		}
		args = append(args, v)
		if p.peek() == ',' {
			p.pos++
		}
	}
	p.pos++
	return p.ev.call(name, args, p.region, p.depth)
}

func (p *parser) array() (value, error) {
	p.pos++
	var out []value
	for p.peek() != ']' {
		if p.peek() == 0 {
			return nil, fmt.Errorf("%w: unclosed array", errBundle)
		}
		p.depth++
		v, err := p.expr()
		p.depth--
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if p.peek() == ',' {
			p.pos++
		}
	}
	p.pos++
	return out, nil
}

func (p *parser) object() (value, error) {
	p.pos++
	out := map[string]value{}
	for p.peek() != '}' {
		start := p.pos
		for p.pos < len(p.src) && p.src[p.pos] != ':' {
			p.pos++
		}
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("%w: unclosed object", errBundle)
		}
		key := strings.TrimSpace(p.src[start:p.pos])
		p.pos++
		p.depth++
		v, err := p.expr()
		p.depth--
		if err != nil {
			return nil, err
		}
		out[key] = v
		if p.peek() == ',' {
			p.pos++
		}
	}
	p.pos++
	return out, nil
}

func numbers(a, b value) (float64, float64, error) {
	x, ok1 := a.(float64)
	y, ok2 := b.(float64)
	if !ok1 || !ok2 {
		return 0, 0, fmt.Errorf("%w: arithmetic on non-numbers", errBundle)
	}
	return x, y, nil
}

// add follows javascript, concatenating when either side is a string
func add(a, b value) (value, error) {
	if x, ok := a.(float64); ok {
		if y, ok := b.(float64); ok {
			return x + y, nil
		}
	}
	as, ok1 := a.(string)
	bs, ok2 := b.(string)
	switch {
	case ok1 && ok2:
		return as + bs, nil
	case ok1:
		return as + text(b), nil
	case ok2:
		return text(a) + bs, nil
	}
	return nil, fmt.Errorf("%w: adding %T and %T", errBundle, a, b)
}

func text(v value) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// stringLiteral reads one javascript string literal at the head of s and
// reports how many bytes it spanned
func stringLiteral(s string) (string, int, error) {
	if s == "" {
		return "", 0, fmt.Errorf("%w: empty string literal", errBundle)
	}
	quote := s[0]
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch c {
		case quote:
			return b.String(), i + 1, nil
		case '\\':
			i++
			if i >= len(s) {
				return "", 0, fmt.Errorf("%w: dangling escape", errBundle)
			}
			switch e := s[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'x':
				if i+2 >= len(s) {
					return "", 0, fmt.Errorf("%w: short hex escape", errBundle)
				}
				n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
				if err != nil {
					return "", 0, fmt.Errorf("%w: hex escape", errBundle)
				}
				b.WriteRune(rune(n))
				i += 2
			case 'u':
				if i+4 >= len(s) {
					return "", 0, fmt.Errorf("%w: short unicode escape", errBundle)
				}
				n, err := strconv.ParseUint(s[i+1:i+5], 16, 16)
				if err != nil {
					return "", 0, fmt.Errorf("%w: unicode escape", errBundle)
				}
				b.WriteRune(rune(n))
				i += 4
			default:
				b.WriteByte(e)
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, fmt.Errorf("%w: unterminated string literal", errBundle)
}

// stringArray reads a javascript array literal of string literals
func stringArray(s string) ([]string, error) {
	if s == "" || s[0] != '[' {
		return nil, fmt.Errorf("%w: string table is not an array", errBundle)
	}
	var out []string
	i := 1
	for i < len(s) {
		switch s[i] {
		case ']':
			return out, nil
		case ',', ' ':
			i++
		case '"', '\'', '`':
			v, n, err := stringLiteral(s[i:])
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			i += n
		default:
			return nil, fmt.Errorf("%w: string table holds %q", errBundle, s[i:min(i+8, len(s))])
		}
		if len(out) > maxTable {
			return nil, fmt.Errorf("%w: string table over %d entries", errBundle, maxTable)
		}
	}
	return nil, fmt.Errorf("%w: unterminated string table", errBundle)
}

// matching finds the brace closing the one at open, skipping string literals
func matching(s string, open int) (int, error) {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '"', '\'', '`':
			_, n, err := stringLiteral(s[i:])
			if err != nil {
				return 0, err
			}
			i += n - 1
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("%w: unbalanced braces", errBundle)
}

// splitTop splits on sep outside brackets and string literals
func splitTop(s string, sep byte) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\'', '`':
			if _, n, err := stringLiteral(s[i:]); err == nil {
				i += n - 1
			}
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
		case sep:
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}
