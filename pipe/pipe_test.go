package pipe

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"testing"
)

func obfuscate(t *testing.T, plain []byte, version string) []byte {
	t.Helper()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	raw := gz.Bytes()
	if version == "2" {
		for i := range raw {
			raw[i] ^= obfKey[i%len(obfKey)]
		}
	}
	return []byte(base64.RawURLEncoding.EncodeToString(raw))
}

func TestDecode(t *testing.T) {
	want := []byte(`{"mappings":{"id":21},"providers":{}}`)
	for _, version := range []string{"1", "2"} {
		got, err := decode(obfuscate(t, want, version), version)
		if err != nil {
			t.Fatalf("version %s: %v", version, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("version %s: got %s want %s", version, got, want)
		}
	}
}
