package miruro

import (
	"context"
	"encoding/json"
	"fmt"
)

// provider is one row of the config resource's provider table
type provider struct {
	// Label is the code the site shows, bee for anikoto and hop for kickassanime
	Label   string `json:"label"`
	Visible bool   `json:"visible"`
	// Relationship is embed for an iframe player over its parent's streams
	Relationship string `json:"relationship"`
}

// providerCodes maps the api's provider ids to the codes the site shows, which
// are what a pin, the history and the preference order name providers by
// the codes outlived the 2026-09 move off the secure pipe, when every provider
// id changed under them
// only a success is kept, since no provider can be named without the table and
// one failed fetch must not cost the rest of the run
func (c *Client) providerCodes(ctx context.Context) (map[string]string, error) {
	c.codesMu.Lock()
	defer c.codesMu.Unlock()
	if c.codes != nil {
		return c.codes, nil
	}

	body, err := c.get(ctx, "/api/config")
	if err != nil {
		return nil, err
	}
	var raw struct {
		Streaming map[string]provider `json:"streaming"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	codes := make(map[string]string, len(raw.Streaming))
	owner := make(map[string]string, len(raw.Streaming))
	for id, p := range raw.Streaming {
		// the site offers neither a hidden provider nor an embed child, and an
		// embed is nothing this program can play
		if !p.Visible || p.Relationship == "embed" {
			continue
		}
		// a provider the table gives no label is known by its id
		code := p.Label
		if code == "" {
			code = id
		}
		// a code shared by two providers would pin neither of them reliably
		if other, dup := owner[code]; dup {
			return nil, fmt.Errorf("miruro config names both %s and %s %s", other, id, code)
		}
		owner[code] = id
		codes[id] = code
	}
	c.codes = codes
	return codes, nil
}
