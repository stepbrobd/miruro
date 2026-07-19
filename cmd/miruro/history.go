package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrg/xdg"

	"ysun.co/miruro"
)

type entry struct {
	AnilistID int             `json:"anilistId"`
	Title     string          `json:"title"`
	Provider  string          `json:"provider"`
	Category  miruro.Category `json:"category"`
	Episode   float64         `json:"episode"`
	Updated   time.Time       `json:"updated"`
}

type store struct {
	path string
}

func openStore() (*store, error) {
	path, err := xdg.StateFile("miruro/history.json")
	if err != nil {
		return nil, err
	}
	return &store{path: path}, nil
}

func (s *store) load() ([]entry, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	// a read error must propagate
	// treating it as empty would let a later save clobber an unreadable file
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var entries []entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// save upserts by AnilistID and keeps the most recent entry first
func (s *store) save(e entry) error {
	e.Updated = time.Now()
	entries, err := s.load()
	if err != nil {
		return err
	}
	kept := entries[:0]
	for _, x := range entries {
		if x.AnilistID != e.AnilistID {
			kept = append(kept, x)
		}
	}
	entries = append([]entry{e}, kept...)

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Clean(s.path))
}

// cacheDir names the segment directory for one episode
// category, provider and quality are all part of the key because each selects a
// different rendition, and reusing one for another would splice a video out of
// the wrong source
func cacheDir(anilistID int, ep float64, category miruro.Category, provider, quality string) (string, error) {
	root, err := xdg.StateFile("miruro/segments")
	if err != nil {
		return "", err
	}
	if quality == "" {
		quality = "best"
	}
	key := fmt.Sprintf("%d-e%s-%s-%s-%s", anilistID, num(ep), category, provider, quality)
	return filepath.Join(root, safeKey(key)), nil
}

// safeKey reduces a cache key to one path component
func safeKey(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		}
		return '_'
	}, s)
}

// clearCache drops every cached segment directory
// the cache is only ever a resumption aid, so removing it can lose progress but
// never a finished download
func clearCache() error {
	root, err := xdg.StateFile("miruro/segments")
	if err != nil {
		return err
	}
	if err := os.RemoveAll(root); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *store) clear() error {
	// an absent history is already the requested end state
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
