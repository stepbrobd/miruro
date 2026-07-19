package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	if os.IsNotExist(err) || len(data) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// save upserts by AnilistID and keeps the most recent entry first.
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

func (s *store) clear() error {
	return os.Remove(s.path)
}
