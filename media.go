package miruro

type Media struct {
	ID       int
	Romaji   string
	English  string
	Episodes int
	// Format is AniList's media format enum, e.g. TV, MOVIE, OVA
	Format string
}

func (m Media) Title() string {
	if m.English != "" {
		return m.English
	}
	return m.Romaji
}
