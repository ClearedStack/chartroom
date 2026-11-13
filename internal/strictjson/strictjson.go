// Package strictjson decodes JSON and refuses anything loose: invalid
// UTF-8, duplicate keys, unknown fields, trailing data, oversized input.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

var (
	// ErrMalformed means the input is not acceptable JSON for the target.
	ErrMalformed = errors.New("malformed JSON")
	// ErrUnknownField means the input has a key the target does not define.
	ErrUnknownField = errors.New("unknown field")
)

const maxDepth = 32

// Check reports whether data is one well-formed JSON object no larger than
// limit bytes, in valid UTF-8, with no duplicate keys and with every key
// written in the contract's lower_snake_case.
func Check(data []byte, limit int) error {
	if len(data) == 0 || len(data) > limit {
		return ErrMalformed
	}
	if !utf8.Valid(data) || !json.Valid(data) {
		return ErrMalformed
	}
	if first := bytes.TrimLeft(data, " \t\r\n"); len(first) == 0 || first[0] != '{' {
		return ErrMalformed
	}
	return walk(data)
}

// Decode checks data with Check and then decodes it into v, refusing any
// field v does not define.
func Decode(data []byte, limit int, v any) error {
	if err := Check(data, limit); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return ErrUnknownField
		}
		return ErrMalformed
	}
	return nil
}

// TopLevel returns the top-level fields of a JSON object without reading
// into them. It refuses what Check refuses, except that only the top-level
// keys have to be unique: what is inside a field is for whoever reads that
// field to check.
func TopLevel(data []byte, limit int) (map[string]json.RawMessage, error) {
	if len(data) == 0 || len(data) > limit || !utf8.Valid(data) || !json.Valid(data) {
		return nil, ErrMalformed
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, ErrMalformed
	}
	out := make(map[string]json.RawMessage)
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return nil, ErrMalformed
		}
		if _, dup := out[key]; dup {
			return nil, ErrMalformed
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, ErrMalformed
		}
		out[key] = value
	}
	return out, nil
}

type frame struct {
	object  bool
	wantKey bool
	keys    map[string]struct{}
}

// walk reads data token by token to find duplicate keys and keys that are
// not in lower_snake_case. encoding/json would silently keep the last
// duplicate and match keys without regard to case.
func walk(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var stack []frame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return ErrMalformed
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				if n := len(stack); n > 0 && stack[n-1].object {
					stack[n-1].wantKey = true
				}
				if len(stack) >= maxDepth {
					return ErrMalformed
				}
				f := frame{object: d == '{', wantKey: d == '{'}
				if f.object {
					f.keys = make(map[string]struct{})
				}
				stack = append(stack, f)
			default:
				stack = stack[:len(stack)-1]
			}
			continue
		}
		n := len(stack)
		if n == 0 || !stack[n-1].object {
			continue
		}
		top := &stack[n-1]
		if !top.wantKey {
			top.wantKey = true
			continue
		}
		key, _ := tok.(string)
		if !contractKey(key) {
			return ErrUnknownField
		}
		if _, dup := top.keys[key]; dup {
			return ErrMalformed
		}
		top.keys[key] = struct{}{}
		top.wantKey = false
	}
}

func contractKey(k string) bool {
	if k == "" || k[0] < 'a' || k[0] > 'z' {
		return false
	}
	for i := 1; i < len(k); i++ {
		c := k[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '_' {
			return false
		}
	}
	return true
}

// Marshal encodes v the one way Chartroom writes JSON: two-space indent,
// no HTML escaping, one trailing newline.
func Marshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		// Only reachable with a type that cannot be encoded, which is a
		// programming error, not a data error.
		panic("strictjson: value cannot be encoded")
	}
	return buf.Bytes()
}
