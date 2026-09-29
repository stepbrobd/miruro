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

// a final segment can hold fewer packets than the sync run asks for, and one
// keeping its decoy failed the cache path's check and the whole download with it
// the decoy's image is what licenses the shorter test, so a body that merely
// ends on a sync byte keeps every byte
func TestNormalizeSegmentStripsAShortDecoyedTail(t *testing.T) {
	// bonk's shape, a 62 byte image padded to 252 bytes before the stream
	decoy := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 252-8)...)
	for _, n := range []int{1, 3, 7} {
		short := append(append([]byte{}, decoy...), tsBlob(n)...)
		if got := normalizeSegment(short); !bytes.Equal(got, tsBlob(n)) {
			t.Errorf("%d packets kept %d decoy bytes", n, len(got)-n*tsPacket)
		}
	}

	// a short segment with no decoy is left as it is
	if got := normalizeSegment(tsBlob(3)); !bytes.Equal(got, tsBlob(3)) {
		t.Error("a short honest segment was modified")
	}

	// no image in front, so a sync byte where the last packet would start is
	// only a coincidence
	body := make([]byte, 40*tsPacket+17)
	body[len(body)-tsPacket] = 0x47
	if got := normalizeSegment(body); !bytes.Equal(got, body) {
		t.Errorf("a body without a decoy lost %d bytes", len(body)-len(got))
	}

	// an image that is not followed by whole packets is not a decoy to strip
	odd := append(append([]byte{}, decoy...), tsBlob(2)[:300]...)
	if got := normalizeSegment(odd); !bytes.Equal(got, odd) {
		t.Errorf("a decoy followed by a torn packet lost %d bytes", len(odd)-len(got))
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

// a body that opens like a decoy but carries no packets anywhere is an image or
// an error page, and is passed on whole rather than cut down to nothing
func TestNormalizeSegmentKeepsAPNGWithoutPackets(t *testing.T) {
	body := append(append([]byte{}, pngMagic...), bytes.Repeat([]byte("IDAT"), 100)...)
	if got := normalizeSegment(body); !bytes.Equal(got, body) {
		t.Errorf("normalizeSegment kept %d of %d bytes, want the body whole", len(got), len(body))
	}
}
