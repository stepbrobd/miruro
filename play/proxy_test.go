package play

import (
	"bytes"
	"math/rand"
	"testing"
)

// tsBlob builds n aligned transport stream packets
func tsBlob(n int) []byte {
	out := make([]byte, n*tsPacket)
	for i := range n {
		out[i*tsPacket] = 0x47
	}
	return out
}

func TestNormalizeSegment(t *testing.T) {
	honest := tsBlob(12)
	if got := normalizeSegment(honest); !bytes.Equal(got, honest) {
		t.Error("honest transport stream was modified")
	}

	prefixed := append([]byte("\x89PNG\r\n\x1a\nHELLO-DECOY"), tsBlob(12)...)
	if got := normalizeSegment(prefixed); !bytes.Equal(got, tsBlob(12)) {
		t.Error("decoy prefix not stripped back to sync")
	}

	junk := []byte("no transport stream in here at all, just text")
	if got := normalizeSegment(junk); !bytes.Equal(got, junk) {
		t.Error("payload with no sync run should pass through")
	}
}

// an encrypted segment is indistinguishable from random bytes, so a short sync
// run would match often enough to truncate ciphertext and break decryption
func TestNormalizeSegmentKeepsRandomPayload(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := range 500 {
		enc := make([]byte, 16*tsPacket)
		r.Read(enc)
		enc[0] = 0
		if got := normalizeSegment(enc); !bytes.Equal(got, enc) {
			t.Fatalf("iteration %d lost %d bytes of ciphertext", i, len(enc)-len(got))
		}
	}
}
