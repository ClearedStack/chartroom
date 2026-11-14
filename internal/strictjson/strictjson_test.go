package strictjson

import (
	"strings"
	"testing"
)

type sample struct {
	Name  string   `json:"name"`
	Inner *sample  `json:"inner,omitempty"`
	List  []sample `json:"list,omitempty"`
}

func TestDecode(t *testing.T) {
	deep := strings.Repeat(`{"inner":`, 40) + `{}` + strings.Repeat(`}`, 40)
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"plain object", `{"name":"a"}`, nil},
		{"nested", `{"name":"a","inner":{"name":"b"},"list":[{"name":"c"},{"name":"d"}]}`, nil},
		{"same key in sibling objects", `{"list":[{"name":"c"},{"name":"c"}]}`, nil},
		{"empty input", ``, ErrMalformed},
		{"not json", `{name}`, ErrMalformed},
		{"array at top", `[{"name":"a"}]`, ErrMalformed},
		{"null at top", `null`, ErrMalformed},
		{"string at top", `"a"`, ErrMalformed},
		{"trailing value", `{"name":"a"} {"name":"b"}`, ErrMalformed},
		{"duplicate key", `{"name":"a","name":"b"}`, ErrMalformed},
		{"duplicate key nested", `{"inner":{"name":"a","name":"b"}}`, ErrMalformed},
		{"duplicate key in list", `{"list":[{"name":"a"},{"name":"a","name":"b"}]}`, ErrMalformed},
		{"duplicate key after nested value", `{"inner":{"name":"a"},"inner":{"name":"b"}}`, ErrMalformed},
		{"wrong type", `{"name":7}`, ErrMalformed},
		{"too deep", deep, ErrMalformed},
		{"invalid utf-8", "{\"name\":\"a\xff\"}", ErrMalformed},
		{"unknown field", `{"name":"a","extra":1}`, ErrUnknownField},
		{"unknown field nested", `{"inner":{"extra":1}}`, ErrUnknownField},
		{"key in another case", `{"Name":"a"}`, ErrUnknownField},
		{"key with a space", `{"na me":"a"}`, ErrUnknownField},
		{"empty key", `{"":"a"}`, ErrUnknownField},
	}
	for _, c := range cases {
		var v sample
		if got := Decode([]byte(c.in), 1<<10, &v); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDecodeSizeLimit(t *testing.T) {
	in := []byte(`{"name":"abcdef"}`)
	var v sample
	if err := Decode(in, len(in), &v); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	if err := Decode(in, len(in)-1, &v); err != ErrMalformed {
		t.Errorf("over the limit: got %v, want %v", err, ErrMalformed)
	}
}

func TestMarshalIsCanonical(t *testing.T) {
	got := string(Marshal(sample{Name: "<a & b>"}))
	want := "{\n  \"name\": \"<a & b>\"\n}\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTopLevel(t *testing.T) {
	top, err := TopLevel([]byte(`{"a":{"x":1,"x":2},"B c":[1,2],"d":null}`), 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 || string(top["a"]) != `{"x":1,"x":2}` || string(top["B c"]) != `[1,2]` || string(top["d"]) != `null` {
		t.Errorf("TopLevel gave %v", top)
	}
	for _, in := range []string{``, `"text"`, `[1]`, `null`, `{"a":1,"a":2}`, `{"a":1} 2`, "{\"a\":\"\xff\"}", `{"a":`} {
		if _, err := TopLevel([]byte(in), 1<<10); err != ErrMalformed {
			t.Errorf("TopLevel(%q) = %v, want %v", in, err, ErrMalformed)
		}
	}
	if _, err := TopLevel([]byte(`{"a":1}`), 3); err != ErrMalformed {
		t.Errorf("over the limit: %v", err)
	}
}
