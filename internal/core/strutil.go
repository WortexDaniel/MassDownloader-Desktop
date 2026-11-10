package core

import (
	"encoding/json"
	"strconv"
)

// Small typed helpers shared by the engine parsers (Str_/StrDflt_ keep the
// any-typed JSON values readable at call sites).

// Str_ renders a JSON any as string ("" for nil/non-strings).
func Str_(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
	}
	return ""
}

// StrDflt_ renders a JSON any as string with a default when empty.
func StrDflt_(v any, d string) string {
	if s := Str_(v); s != "" {
		return s
	}
	return d
}
