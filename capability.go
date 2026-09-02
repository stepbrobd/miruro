package miruro

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
