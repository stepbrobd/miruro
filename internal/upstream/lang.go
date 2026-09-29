package upstream

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Language is the tag a subtitle track is in, read from what a provider sent
// several providers send the first two letters of the language's English name
// as its code, po for both Polish and Portuguese, sp for Spanish and ch for
// Chinese, and older hop sources sent the first two of its native name, ti
// for Tiếng Việt, so the label decides where it names a language, and the
// code only where it does not
// a code agreeing with the label is kept as sent, since it can carry a script
// or region the label spells out, zh-hans for 中文（简体）
func Language(code, label string) string {
	code = strings.TrimSpace(code)
	named := labeled(label)
	switch {
	case named == "":
		return shortCode(code)
	case strings.EqualFold(primary(code), named):
		return code
	default:
		return named
	}
}

// labeled is the tag of the language a label opens with, empty when it opens
// with none this knows
// a name only counts as a whole word, so Englishman is no English track
func labeled(label string) string {
	label = strings.ToLower(strings.TrimSpace(label))
	for _, ext := range []string{".vtt", ".srt", ".ass", ".ssa"} {
		label = strings.TrimSuffix(label, ext)
	}
	best, tag := 0, ""
	for name, t := range languageNames {
		if len(name) <= best || !strings.HasPrefix(label, name) {
			continue
		}
		if next, _ := utf8.DecodeRuneInString(label[len(name):]); label[len(name):] != "" && (unicode.IsLetter(next) || unicode.IsMark(next)) {
			continue
		}
		best, tag = len(name), t
	}
	return tag
}

// shortCode maps an ISO 639-2 code to the ISO 639-1 one a player and --lang
// expect, keeping any other code as sent
func shortCode(code string) string {
	if t, ok := threeLetter[strings.ToLower(code)]; ok {
		return t
	}
	return code
}

// languageNames maps the names providers label tracks with, English and
// native, to their tags
var languageNames = map[string]string{
	"english": "en",
	"arabic":  "ar", "العربية": "ar",
	"bengali": "bn", "বাংলা": "bn",
	"bulgarian": "bg", "български": "bg",
	"catalan": "ca", "català": "ca",
	"chinese": "zh", "中文": "zh", "简体中文": "zh", "繁體中文": "zh", "繁体中文": "zh",
	"croatian": "hr", "hrvatski": "hr",
	"czech": "cs", "čeština": "cs", "cestina": "cs",
	"danish": "da", "dansk": "da",
	"dutch": "nl", "nederlands": "nl",
	"filipino": "tl", "tagalog": "tl",
	"finnish": "fi", "suomi": "fi",
	"french": "fr", "français": "fr", "francais": "fr",
	"german": "de", "deutsch": "de",
	"greek": "el", "ελληνικά": "el",
	"hebrew": "he", "עברית": "he",
	"hindi": "hi", "हिन्दी": "hi",
	"hungarian": "hu", "magyar": "hu",
	"indonesian": "id", "bahasa indonesia": "id",
	"italian": "it", "italiano": "it",
	"japanese": "ja", "日本語": "ja",
	"korean": "ko", "한국어": "ko",
	"malay": "ms", "bahasa melayu": "ms", "bahasa malaysia": "ms",
	"norwegian": "no", "norsk": "no",
	"persian": "fa", "فارسی": "fa",
	"polish": "pl", "polski": "pl",
	"portuguese": "pt", "português": "pt", "portugues": "pt",
	"romanian": "ro", "română": "ro", "romana": "ro",
	"russian": "ru", "русский": "ru",
	"serbian": "sr", "српски": "sr",
	"slovak": "sk", "slovenčina": "sk",
	"spanish": "es", "español": "es", "espanol": "es", "castilian": "es",
	"swedish": "sv", "svenska": "sv",
	"tamil": "ta", "தமிழ்": "ta",
	"telugu": "te",
	"thai":   "th", "ภาษาไทย": "th", "ไทย": "th",
	"turkish": "tr", "türkçe": "tr", "turkce": "tr",
	"ukrainian": "uk", "українська": "uk",
	"vietnamese": "vi", "tiếng việt": "vi", "tieng viet": "vi",
}

// threeLetter maps the ISO 639-2 codes providers send, bibliographic and
// terminological alike, to ISO 639-1
var threeLetter = map[string]string{
	"eng": "en", "ara": "ar", "ben": "bn", "bul": "bg", "cat": "ca",
	"chi": "zh", "zho": "zh", "hrv": "hr", "cze": "cs", "ces": "cs",
	"dan": "da", "dut": "nl", "nld": "nl", "tgl": "tl", "fil": "tl",
	"fin": "fi", "fre": "fr", "fra": "fr", "ger": "de", "deu": "de",
	"gre": "el", "ell": "el", "heb": "he", "hin": "hi", "hun": "hu",
	"ind": "id", "ita": "it", "jpn": "ja", "kor": "ko", "may": "ms",
	"msa": "ms", "nor": "no", "nob": "no", "per": "fa", "fas": "fa",
	"pol": "pl", "por": "pt", "rum": "ro", "ron": "ro", "rus": "ru",
	"srp": "sr", "slo": "sk", "slk": "sk", "spa": "es", "swe": "sv",
	"tam": "ta", "tel": "te", "tha": "th", "tur": "tr", "ukr": "uk",
	"vie": "vi",
}
