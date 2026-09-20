package miruro

import (
	"context"
	"encoding/json"

	"ysun.co/miruro/internal/upstream"
)

// Capabilities fetches the capability table, once per client
// a failure is remembered, so a run that cannot reach the resource treats every
// provider as undeclared instead of refetching per episode
// a canceled run is not remembered, since the next one would inherit a verdict
// about nothing that was ever attempted
func (c *Client) Capabilities(ctx context.Context) (upstream.Capabilities, error) {
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

func (c *Client) config(ctx context.Context) (upstream.Capabilities, error) {
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

	cfg := make(upstream.Capabilities, len(raw.Streaming))
	for code, p := range raw.Streaming {
		cfg[code] = upstream.Caps{
			Hard:  p.Capabilities.Sub,
			Soft:  p.Capabilities.Ssub,
			Embed: p.Player == "iframe" || p.Relationship == "embed",
		}
	}
	return cfg, nil
}
