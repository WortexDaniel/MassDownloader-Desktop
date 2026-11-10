// Modular filter specifications — reusable, engine-agnostic, UI-free.
// Port of app/core/filters/*.py.
//
// Each filter kind mirrors one Python dataclass. An engine composes its
// sidebar from these specs in its own filters.go; a spec knows only its
// label, Kind and how its value maps to booru-style query tokens — engines
// that don't speak that grammar (Pixiv) parse the tokens back.
package core

import (
	"regexp"
	"strings"
)

// FilterKind enumerates the widget shapes the frontend renders.
type FilterKind string

const (
	KindAuthors   FilterKind = "authors"
	KindTags      FilterKind = "tags"
	KindExclude   FilterKind = "exclude"
	KindThreshold FilterKind = "threshold"
	KindChoice    FilterKind = "choice"
	KindText      FilterKind = "text"
	KindBool      FilterKind = "bool"
)

// Filter is the tagged union of all seven spec kinds. Field names follow the
// Python dataclasses; zero values mean "use the default".
type Filter struct {
	Kind FilterKind `json:"kind"`

	Key         string `json:"key"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder,omitempty"`
	// Autocomplete selects the TagEditor mode ("tags"/"artists"/"").
	Autocomplete string `json:"autocomplete,omitempty"`
	Accent       string `json:"accent,omitempty"`
	Tooltip      string `json:"tooltip,omitempty"`

	// Threshold fields.
	Token        string `json:"token,omitempty"`
	DefaultValue int64  `json:"default_value,omitempty"`
	Minimum      int64  `json:"minimum,omitempty"`
	Maximum      int64  `json:"maximum,omitempty"`
	Ops          string `json:"ops,omitempty"`       // display ops, e.g. "≥ > ="
	OpTokens     string `json:"op_tokens,omitempty"` // e.g. ">= > ="

	// Choice fields.
	Options []ChoiceOption `json:"options,omitempty"`
	// ValueMap maps canonical value -> engine token (Gelbooru e->explicit).
	ValueMap map[string]string `json:"value_map,omitempty"`

	// Bool field: the default on/off state.
	DefaultBool bool   `json:"default_bool,omitempty"`
	FalseToken  string `json:"false_token,omitempty"`
	EnabledIf   string `json:"enabled_if,omitempty"`
	// LocalOnly controls media handling without adding search constraints.
	LocalOnly bool `json:"local_only,omitempty"`

	// Text field token prefix (e.g. "id").
}

// ChoiceOption is one (label, value) pair of a choice filter; Value is the
// canonical key so it carries over when switching sites; "" means no
// constraint.
type ChoiceOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// DefaultOps/DefaultOpTokens mirror the Python threshold defaults.
const (
	DefaultOps      = "≥ > ="
	DefaultOpTokens = ">= > ="
)

// NewTags builds TagsFilter with Python's defaults.
func NewTags() *Filter {
	return &Filter{Kind: KindTags, Key: "include", Label: "Теги — обязательные",
		Placeholder: "добавить тег… (пусто — любые)", Autocomplete: "tags"}
}

// NewExclude builds ExcludeFilter ("emits -tag").
func NewExclude() *Filter {
	return &Filter{Kind: KindExclude, Key: "exclude", Label: "Теги — исключить",
		Placeholder: "исключить тег…", Autocomplete: "tags"}
}

// NewAuthors builds AuthorsFilter (multiple authors OR-ed with ~).
func NewAuthors() *Filter {
	return &Filter{Kind: KindAuthors, Key: "artists", Label: "Авторы (любой из)",
		Placeholder: "имя автора… (пусто — все)", Autocomplete: "artists"}
}

// NewThreshold builds ThresholdFilter.
func NewThreshold(key, label, token string, defVal int64) *Filter {
	return &Filter{Kind: KindThreshold, Key: key, Label: label, Token: token,
		DefaultValue: defVal, Minimum: -1000, Maximum: 1_000_000,
		Ops: DefaultOps, OpTokens: DefaultOpTokens}
}

// NewRating builds the default rating ChoiceFilter.
func NewRating() *Filter {
	return &Filter{Kind: KindChoice, Key: "rating", Label: "Рейтинг",
		Token: "rating",
		Options: []ChoiceOption{
			{"Любой", ""}, {"Safe", "s"}, {"Questionable", "q"}, {"Explicit", "e"},
		}}
}

// NewSort builds the upload-time sort ChoiceFilter used by booru engines.
func NewSort() *Filter {
	return &Filter{Kind: KindChoice, Key: "sort", Label: "Сортировка",
		Token: "order",
		Options: []ChoiceOption{
			{"Сначала новые", ""}, {"Сначала старые", "id"},
		},
		Tooltip: "порядок по времени загрузки"}
}

// NewText builds TextFilter (token:<chunk> per whitespace/comma chunk).
func NewText(key, label, placeholder, token string) *Filter {
	return &Filter{Kind: KindText, Key: key, Label: label,
		Placeholder: placeholder, Token: token}
}

// NewBool builds BoolFilter (emits Token verbatim when checked).
func NewBool(key, label, token string, def bool) *Filter {
	return &Filter{Kind: KindBool, Key: key, Label: label, Token: token,
		DefaultBool: def}
}

// ImagesOnlyFilter returns the shared «Только изображения» switch, declared
// per engine so each site can word it for itself (or omit it). It emits
// IMAGES_ONLY_TOKEN; the client honours that flag for every site. On by
// default (the old global default).
func ImagesOnlyFilter(label, tooltip string) *Filter {
	return NewBool("images_only", label, IMAGES_ONLY_TOKEN, true)
}

func GroupAlbumsFilter() *Filter {
	f := NewBool("group_albums", "Группировать альбомы", "", false)
	f.LocalOnly = true
	f.Tooltip = "Сохранять файлы альбома в подпапку автора с названием поста. Если названия нет — использовать ID поста."
	return f
}

// FilterList orders the specs for an engine or site override.
type FilterList []*Filter

// Find returns the spec with the given key, or nil.
func (l FilterList) Find(key string) *Filter {
	for _, f := range l {
		if f.Key == key {
			return f
		}
	}
	return nil
}

// Tokens converts a filter value into query tokens. The value shape follows
// the Python module:
//
//	tags / exclude / authors -> []string
//	threshold                -> {Enabled bool, Op string, N int64}
//	choice / text            -> string
//	bool                     -> bool
func (f *Filter) Tokens(value any) []string {
	if f.LocalOnly {
		return nil
	}
	switch f.Kind {
	case KindTags:
		return toStrings(value)
	case KindExclude:
		var out []string
		for _, t := range toStrings(value) {
			out = append(out, "-"+t)
		}
		return out
	case KindAuthors:
		v := toStrings(value)
		if len(v) == 1 {
			return v
		}
		out := make([]string, 0, len(v))
		for _, a := range v {
			out = append(out, "~"+a)
		}
		return out
	case KindThreshold:
		m, _ := value.(map[string]any)
		if list, ok := value.([]any); ok && len(list) >= 3 {
			m = map[string]any{"enabled": list[0], "op": list[1], "n": list[2]}
		}
		if m == nil || !truthy(m["enabled"]) {
			return nil
		}
		op := str(m["op"])
		if op == "=" {
			op = ""
		}
		n := AsInt(m["n"])
		return []string{fmtToken(f.Token, op, n)}
	case KindChoice:
		s := strings.TrimSpace(str(value))
		if s == "" {
			return nil
		}
		emit := s
		if f.ValueMap != nil {
			if mapped, ok := f.ValueMap[s]; ok {
				emit = mapped
			}
		}
		if f.Token == "" {
			return []string{emit}
		}
		return []string{f.Token + ":" + emit}
	case KindText:
		var out []string
		for _, c := range textChunkRe.Split(strings.TrimSpace(str(value)), -1) {
			if c != "" {
				out = append(out, f.Token+":"+c)
			}
		}
		return out
	case KindBool:
		if truthy(value) {
			return []string{f.Token}
		}
		if f.FalseToken != "" {
			return []string{f.FalseToken}
		}
		return nil
	}
	return nil
}

// DefaultVal returns the JSON-shaped default value for the panel store.
func (f *Filter) DefaultVal() any {
	switch f.Kind {
	case KindTags, KindExclude, KindAuthors:
		return []string{}
	case KindThreshold:
		op := firstWord(f.OpTokens, ">=")
		return map[string]any{"enabled": false, "op": op, "n": f.DefaultValue}
	case KindChoice:
		if len(f.Options) > 0 {
			return f.Options[0].Value
		}
		return ""
	case KindText:
		return ""
	case KindBool:
		return f.DefaultBool
	}
	return nil
}

func fmtToken(token, op string, n int64) string {
	if token == "" {
		return ""
	}
	return token + ":" + op + strconvI(n)
}

func strconvI(n int64) string {
	// small helper to avoid importing strconv twice in this file body
	b := [20]byte{}
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			break
		}
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func toStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			out = append(out, str(x))
		}
		return out
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	}
	return nil
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	}
	return ""
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case jsonNumber:
		n, _ := t.Int64()
		return n != 0
	case string:
		return t != ""
	}
	return false
}

func firstWord(s, dflt string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return dflt
	}
	return f[0]
}

var textChunkRe = regexp.MustCompile(`[\s,]+`)
