// Package compact trims upstream payloads for the agent's context without losing
// what it acts on: long strings are cut, long arrays capped, deep trees pruned,
// hypermedia noise dropped. The trimmed shape says where it was cut.
package compact

import (
	"encoding/json"
	"fmt"
)

type Options struct {
	MaxString int      // runes per string value (0 = default 400)
	MaxItems  int      // items per array (0 = default 50)
	MaxDepth  int      // nesting depth (0 = default 6)
	DropKeys  []string // object keys removed outright (default: _links, _embedded)
}

func (o Options) withDefaults() Options {
	if o.MaxString <= 0 {
		o.MaxString = 400
	}
	if o.MaxItems <= 0 {
		o.MaxItems = 50
	}
	if o.MaxDepth <= 0 {
		o.MaxDepth = 6
	}
	if o.DropKeys == nil {
		o.DropKeys = []string{"_links", "_embedded"}
	}
	return o
}

// Trim returns a trimmed copy of a decoded JSON value.
func Trim(v any, o Options) any {
	return trim(v, o.withDefaults(), 0)
}

func trim(v any, o Options, depth int) any {
	switch t := v.(type) {
	case string:
		r := []rune(t)
		if len(r) > o.MaxString {
			return string(r[:o.MaxString]) + fmt.Sprintf("… [%d more chars]", len(r)-o.MaxString)
		}
		return t
	case []any:
		if depth >= o.MaxDepth {
			return fmt.Sprintf("[array of %d]", len(t))
		}
		n := len(t)
		if n > o.MaxItems {
			n = o.MaxItems
		}
		out := make([]any, 0, n+1)
		for _, x := range t[:n] {
			out = append(out, trim(x, o, depth+1))
		}
		if len(t) > n {
			out = append(out, fmt.Sprintf("… [%d more items]", len(t)-n))
		}
		return out
	case map[string]any:
		if depth >= o.MaxDepth {
			return fmt.Sprintf("{object with %d keys}", len(t))
		}
		out := make(map[string]any, len(t))
		for k, x := range t {
			if dropped(k, o.DropKeys) {
				continue
			}
			out[k] = trim(x, o, depth+1)
		}
		return out
	default:
		return v
	}
}

func dropped(key string, keys []string) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

// Pick keeps the listed keys of an object, or of each object in a list; other values pass through.
func Pick(v any, fields []string) any {
	if len(fields) == 0 {
		return v
	}
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for _, f := range fields {
			if x, ok := t[f]; ok {
				out[f] = x
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, x := range t {
			out = append(out, Pick(x, fields))
		}
		return out
	}
	return v
}

// Size is the serialised length, for receipts.
func Size(v any) int64 {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return int64(len(b))
}
