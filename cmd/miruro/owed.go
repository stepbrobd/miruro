package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/adrg/xdg"
	"github.com/charmbracelet/log"

	"ysun.co/miruro/internal/upstream"
)

// owed is an episode on disk that some of its sidecars never reached, kept so
// a rerun can fetch them from the rendition its video came from
// the rendition of a file on disk is otherwise unknown, and a soft sidecar over
// a hardsub, or one timed to another provider's cut, is worse than none
type owed struct {
	Video    string            `json:"video"`
	Provider string            `json:"provider"`
	Category upstream.Category `json:"category"`
	// Server is the stream the video came from, whose referer the sidecars take
	Server string `json:"server"`
}

// owedPath names the record of the video at path, hashed since a path does not
// fit in a file name
func owedPath(video string) string {
	if abs, err := filepath.Abs(video); err == nil {
		video = abs
	}
	sum := sha256.Sum256([]byte(video))
	return filepath.Join(xdg.StateHome, "miruro", "sidecars", hex.EncodeToString(sum[:8])+".json")
}

// owing is the record of the video at path, false when it owes nothing or its
// record cannot be read
func owing(video string) (owed, bool) {
	data, err := os.ReadFile(owedPath(video))
	if err != nil {
		return owed{}, false
	}
	var o owed
	if err := json.Unmarshal(data, &o); err != nil || o.Provider == "" {
		return owed{}, false
	}
	return o, true
}

// record keeps what the video is owed, replacing any earlier record of it
func (o owed) record() error {
	path := owedPath(o.Video)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// settle drops the record of the video at path once it owes nothing
// a record that outlives its debt costs a rerun one resolution, so a failure
// here is warned rather than failing an episode that is whole
func settle(video string) {
	if err := os.Remove(owedPath(video)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn("record of missing subtitles not cleared", "video", video, "err", err)
	}
}
