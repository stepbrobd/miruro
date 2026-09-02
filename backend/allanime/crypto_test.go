package allanime

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// the mask and the signature are pinned to what the site's own code produced
// for the fixture build in node, so a drift in either derivation shows here
// before it shows as a refused bootstrap
func TestMaskAndBoot(t *testing.T) {
	b, err := parseBuild(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(b.mask()); got != "c62ce6fa35d872a77e1c94139ba4a4906883d8221f8681b7829d27a81f413106" {
		t.Errorf("mask = %s", got)
	}
	if got := b.boot("mkissa.to", "mkissa", "k7", 2921); got != "707f9467543c86118bee836c0ca8dfbfd62b98bb3fc6413d7c06a509c424afd9" {
		t.Errorf("boot = %s", got)
	}
}

func TestSessionKey(t *testing.T) {
	b := &build{ID: "1", frags: [][]byte{make([]byte, 8), make([]byte, 8), make([]byte, 8), make([]byte, 8)}}
	if _, err := b.sessionKey(make([]byte, keyLen-1)); err == nil {
		t.Error("short bootstrap key accepted")
	}
	partB := bytes.Repeat([]byte{0xff}, keyLen)
	key, err := b.sessionKey(partB)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range b.mask() {
		if key[i] != m^0xff {
			t.Fatalf("key[%d] = %x, want %x", i, key[i], m^0xff)
		}
	}
}

func TestTokenRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, keyLen)
	now := time.UnixMilli(1788346800000 + 123456)
	tok, err := token(key, 2956, "153", "abc", "k7", now)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := open(key, tok)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		V       int    `json:"v"`
		TS      int64  `json:"ts"`
		Epoch   int64  `json:"epoch"`
		BuildID string `json:"buildId"`
		QH      string `json:"qh"`
		K       string `json:"k"`
	}
	if err := json.Unmarshal(plain, &got); err != nil {
		t.Fatal(err)
	}
	if got.V != 1 || got.TS != 1788346800000 || got.Epoch != 2956 || got.BuildID != "153" || got.QH != "abc" || got.K != "k7" {
		t.Errorf("payload = %s", plain)
	}
	// the same inputs in the same window seal to the same token, since the
	// nonce is derived, and a later window moves it
	again, _ := token(key, 2956, "153", "abc", "k7", now.Add(time.Minute))
	if again != tok {
		t.Error("token changed inside its window")
	}
	later, _ := token(key, 2956, "153", "abc", "k7", now.Add(window))
	if later == tok {
		t.Error("token did not move with the window")
	}
}

func TestOpenRefuses(t *testing.T) {
	key := bytes.Repeat([]byte{7}, keyLen)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := bytes.Repeat([]byte{1}, nonceLen)
	sealed := gcm.Seal(nil, nonce, []byte(`{"ok":true}`), nil)
	good := append(append([]byte{blobVersion}, nonce...), sealed...)

	for name, blob := range map[string]string{
		"not base64":    "%%%",
		"short":         base64.StdEncoding.EncodeToString([]byte{1, 2, 3}),
		"other version": base64.StdEncoding.EncodeToString(append([]byte{2}, good[1:]...)),
		"tampered":      base64.StdEncoding.EncodeToString(append(good[:len(good)-1:len(good)-1], good[len(good)-1]^1)),
		"other key":     base64.StdEncoding.EncodeToString(good),
	} {
		k := key
		if name == "other key" {
			k = bytes.Repeat([]byte{8}, keyLen)
		}
		if _, err := open(k, blob); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
	plain, err := open(key, base64.StdEncoding.EncodeToString(good))
	if err != nil || !strings.Contains(string(plain), "ok") {
		t.Errorf("good blob = %q, %v", plain, err)
	}
}
