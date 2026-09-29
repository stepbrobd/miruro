package upstream

import "testing"

// every row is a code and label a provider sent on 2026-09-29, or on 2026-09-23
// for hop's older source, and the tag a player and --lang should see
func TestLanguage(t *testing.T) {
	for _, tc := range []struct{ code, label, want string }{
		// bee and sun send the first two letters of the English name
		{"po", "Polish (Polish - [Full])", "pl"},
		{"po", "Portuguese (- Portuguese(Brazil))", "pt"},
		{"sp", "Spanish (- Castilian [Full])", "es"},
		{"ch", "Chinese (Chinese - (Simplified) [Full])", "zh"},
		{"du", "Dutch (Dutch - [Full])", "nl"},
		{"sw", "Swedish (Swedish - [Full])", "sv"},
		{"tu", "Turkish", "tr"},
		{"ge", "German.vtt", "de"},
		{"in", "Indonesian.vtt", "id"},
		{"ma", "Malay.vtt", "ms"},
		{"ba", "Bahasa Indonesia.vtt", "id"},
		{"sp", "Spanish - Spanish Latin America.vtt", "es"},
		// hop's older source sent the first two letters of the native name
		{"ti", "Tiếng Việt", "vi"},
		{"ba", "Bahasa Melayu", "ms"},
		{"ภา", "ภาษาไทย", "th"},
		// a code agreeing with its label is kept, script and region included
		{"zh-hans", "中文（简体）", "zh-hans"},
		{"pt-BR", "Portuguese (Brazil)", "pt-BR"},
		{"en", "English 2", "en"},
		{"ja", "Japanese (Japanese - [SDH])", "ja"},
		// three letter codes become two, and so does one whose label is only a code
		{"eng", "English", "en"},
		{"ara", "", "ar"},
		{"en", "ENG", "en"},
		// a label naming no language leaves the code alone
		{"su", "Subtitles.vtt", "su"},
		{"fr", "", "fr"},
		{"", "", ""},
		// a name counts only as a whole word
		{"xx", "Englishman", "xx"},
	} {
		if got := Language(tc.code, tc.label); got != tc.want {
			t.Errorf("Language(%q, %q) = %q, want %q", tc.code, tc.label, got, tc.want)
		}
	}
}

// a rendition names its language in ISO 639-2 where an embed names it as a
// BCP 47 tag, and the two have to meet
func TestSameLanguage(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"eng", "en-US", true},
		{"jpn", "ja-JP", true},
		{"ENG", "en", true},
		{"ger", "de", true},
		{"eng", "ja-JP", false},
		{"", "", false},
		{"en", "", false},
	} {
		if got := SameLanguage(tc.a, tc.b); got != tc.want {
			t.Errorf("SameLanguage(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
