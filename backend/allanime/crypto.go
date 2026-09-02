package allanime

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// errCrypto marks a request the site's attestation refused or a payload this
// client cannot open
var errCrypto = errors.New("allanime attestation failed")

const (
	keyLen = 32
	// window is the bucket a token timestamp is rounded down to
	window = 5 * time.Minute
	// blobVersion leads every sealed payload in either direction
	blobVersion = 1
	nonceLen    = 12
)

// salt spreads the build id over a key-length buffer
func (b *build) salt() []byte {
	out := make([]byte, keyLen)
	for i := range out {
		out[i] = b.ID[i%len(b.ID)] ^ byte((i*b.saltMul+b.saltAdd)&0xff)
	}
	return out
}

// mask is the client half of the session key, assembled from the fragments
// the bundle carries and the build id, which is what ties a key to a deploy
func (b *build) mask() []byte {
	s := b.salt()
	out := make([]byte, keyLen)
	for i, f := range b.frags {
		for j := range f {
			at := i*fragLen + j
			out[at] = f[j] ^ s[at] ^ byte((i*b.fragMul+j*b.fragAdd)&0xff)
		}
	}
	return out
}

// boot signs a bootstrap request
// two rounds of hmac keyed on the mask, the first over the prefixed build id
// and the second over the request's identity in the order the bundle names
func (b *build) boot(host, group, lane string, epoch int64) string {
	inner := hmacSHA256(b.mask(), []byte(b.prefix+b.ID))
	field := map[string]string{
		"host":    host,
		"epoch":   strconv.FormatInt(epoch, 10),
		"group":   group,
		"lane":    lane,
		"buildId": b.ID,
	}
	parts := make([]string, len(b.parts))
	for i, p := range b.parts {
		parts[i] = field[p]
	}
	return hex.EncodeToString(hmacSHA256(inner, []byte(strings.Join(parts, b.join))))
}

func hmacSHA256(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil)
}

// sessionKey joins the server half handed over by the bootstrap with the mask
func (b *build) sessionKey(partB []byte) ([]byte, error) {
	if len(partB) < keyLen {
		return nil, fmt.Errorf("%w: bootstrap key of %d bytes", errCrypto, len(partB))
	}
	m := b.mask()
	key := make([]byte, keyLen)
	for i := range key {
		key[i] = partB[i] ^ m[i%len(m)]
	}
	return key, nil
}

// token seals the request attestation the api checks before it resolves an
// episode
// the nonce is derived rather than random because the server recomputes it
// from the same fields to open the token
func token(key []byte, epoch int64, buildID, qh, lane string, now time.Time) (string, error) {
	ts := now.UnixMilli() / window.Milliseconds() * window.Milliseconds()
	payload := fmt.Sprintf(`{"v":1,"ts":%d,"epoch":%d,"buildId":%q,"qh":%q,"k":%q}`, ts, epoch, buildID, qh, lane)
	seed := sha256.Sum256(fmt.Appendf(nil, "%d:%s:%s:%d:%s", epoch, buildID, qh, ts, lane))
	gcm, err := gcm(key)
	if err != nil {
		return "", err
	}
	nonce := seed[:nonceLen]
	out := append([]byte{blobVersion}, nonce...)
	out = gcm.Seal(out, nonce, []byte(payload), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

// open unseals a payload the api answered with
func open(key []byte, blob string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return nil, fmt.Errorf("%w: payload is not base64", errCrypto)
	}
	if len(raw) < 1+nonceLen {
		return nil, fmt.Errorf("%w: payload of %d bytes", errCrypto, len(raw))
	}
	if raw[0] != blobVersion {
		return nil, fmt.Errorf("%w: payload version %d", errCrypto, raw[0])
	}
	gcm, err := gcm(key)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, raw[1:1+nonceLen], raw[1+nonceLen:], nil)
	if err != nil {
		return nil, fmt.Errorf("%w: payload does not open", errCrypto)
	}
	return plain, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errCrypto, err)
	}
	return cipher.NewGCM(block)
}
