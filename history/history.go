// Package history persists the last watched episode per anime for resume.
package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/adrg/xdg"

	"ysun.co/miruro/catalog"
)

type Entry struct {
	AnilistID int              `json:"anilistId"`
	Title     string           `json:"title"`
	Provider  string           `json:"provider"`
	Category  catalog.Category `json:"category"`
	Episode   float64          `json:"episode"`
	Updated   time.Time        `json:"updated"`
}

type Store struct {
	path string
}

func Open() (*Store, error) {
	path, err := xdg.StateFile("miruro/history.json")
	if err != nil {
		return nil, err
	}
	return &Store{path: path}, nil
}

func (s *Store) Load() ([]Entry, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// Save upserts by AnilistID and keeps the most recent entry first.
func (s *Store) Save(e Entry) error {
	e.Updated = time.Now()
	entries, err := s.Load()
	if err != nil {
		return err
	}
	kept := entries[:0]
	for _, x := range entries {
		if x.AnilistID != e.AnilistID {
			kept = append(kept, x)
		}
	}
	entries = append([]Entry{e}, kept...)
	return s.write(entries)
}

func (s *Store) Clear() error {
	return os.Remove(s.path)
}

func (s *Store) write(entries []Entry) error {
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
