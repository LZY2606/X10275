package jsondiff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func cannon(b []byte) []byte {
	var v any
	_ = json.Unmarshal(b, &v)
	c, _ := json.Marshal(v)
	return c
}

func TestScratchExp(t *testing.T) {
	cases := []struct {
		name     string
		src, tgt string
		opts     []Option
	}{
		{"dup-equiv-multiset-equal", `[1,1,2]`, `[2,1,1]`, []Option{Equivalent()}},
		{"dup-equiv-multiset-differ", `[1,1,2]`, `[1,2,2]`, []Option{Equivalent()}},
		{"dup-noopt", `[1,1,2]`, `[1,2,2]`, nil},
		{"num-repr-default", `1`, `1.0`, nil},
		{"negzero", `-0`, `0`, nil},
		{"bignum", `9007199254740993`, `9007199254740992`, nil},
		{"root-replace", `[1,2,3]`, `{"a":1}`, nil},
		{"move", `{"a":1}`, `{"b":1}`, []Option{Factorize()}},
		{"copy", `[{"a":1},{"b":2}]`, `[{"a":1},{"b":2},{"a":1}]`, []Option{Factorize()}},
		{"copy-invertible", `[{"a":1},{"b":2}]`, `[{"a":1},{"b":2},{"a":1}]`, []Option{Factorize(), Invertible()}},
		{"invertible", `{"a":1}`, `{"a":2}`, []Option{Invertible()}},
		{"reorder", `{"z":1,"a":2,"m":3}`, `{"z":1,"a":9,"m":3}`, nil},
		{"dup-lcs", `[1,1,2]`, `[1,2,1]`, []Option{LCS()}},
		{"dup-lcs-factorize", `[1,1,2]`, `[1,2,1]`, []Option{LCS(), Factorize()}},
		{"dup-factorize", `[1,1,2]`, `[1,2,2]`, []Option{Factorize()}},
	}
	for _, c := range cases {
		p, err := CompareJSON([]byte(c.src), []byte(c.tgt), c.opts...)
		fmt.Printf("== %s\n  err=%v\n  patch=%s\n", c.name, err, p)
		if err == nil {
			out, aerr := p.apply([]byte(c.src), false)
			fmt.Printf("  apply err=%v out=%s eq=%v\n", aerr, out, bytes.Equal(cannon(out), cannon([]byte(c.tgt))))
		}
	}
}
