package miruro

import (
	"context"
	"encoding/json"
)

// Caps is the set of renditions a provider declares it can serve
// the api names the burned-in one "sub" and the detachable one "ssub", which
// reads backwards here, so both are renamed at the parse boundary and nowhere
// else
type Caps struct {
	// Hard means the subtitles arrive burned into the picture
	Hard bool
	// Soft means the provider ships a subtitle file alongside the stream
	Soft bool
	// Embed means an iframe player, which nothing here can play
	Embed bool
}

// Capabilities is the provider capability table, keyed by provider code.
// A code the table does not name is undeclared rather than incapable, since the
// episodes resource serves providers the config resource omits
type Capabilities map[string]Caps

// Capabilities fetches the capability table, once per client.
// A failure is remembered, so a run that cannot reach the resource treats every
// provider as undeclared instead of refetching per episode
// a cancelled run is not remembered, since the next one would inherit a verdict
// about nothing that was ever attempted
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	c.cfgMu.Lock()
	defer c.cfgMu.Unlock()
	if c.cfgDone {
		return c.cfg, c.cfgErr
	}
	cfg, err := c.config(ctx)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c.cfg, c.cfgErr, c.cfgDone = cfg, err, true
	return cfg, err
}

func (c *Client) config(ctx context.Context) (Capabilities, error) {
	body, err := c.pipe(ctx, "config", nil)
	if err != nil {
		return nil, err
	}

	var raw struct {
		Streaming map[string]struct {
			Capabilities struct {
				Sub  bool `json:"sub"`
				Ssub bool `json:"ssub"`
			} `json:"capabilities"`
			Player       string `json:"player"`
			Relationship string `json:"relationship"`
		} `json:"streaming"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	cfg := make(Capabilities, len(raw.Streaming))
	for code, p := range raw.Streaming {
		cfg[code] = Caps{
			Hard:  p.Capabilities.Sub,
			Soft:  p.Capabilities.Ssub,
			Embed: p.Player == "iframe" || p.Relationship == "embed",
		}
	}
	return cfg, nil
}
