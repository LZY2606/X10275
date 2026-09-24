package jsondiff

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestScratchExp3(t *testing.T) {
	// panic via invalid type in CompareWithoutMarshal (error-layer path)

	d2 := &Differ{}
	_, err2 := func() (p Patch, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		d2.WithOpts()
		// a ok first, then bad tgt value
		d2.Compare(map[string]any{"a": 1.0, "bad": 1.0}, map[string]any{"a": 2.0, "bad": bad})
		return d2.Patch(), nil
	}()
	fmt.Printf("direct panic err2=%v leftover patch=%s\n", err2, d2.Patch())
	d2.Reset()
	d2.Compare(map[string]any{"a": 1.0}, map[string]any{"a": 2.0})
	fmt.Printf("after reset retry: %s\n", d2.Patch())

	// rationalize behavior
	p, _ := CompareJSON([]byte(`{"a":{"b":1,"c":2}}`), []byte(`{"a":{"b":3,"c":4}}`), Rationalize())
	fmt.Printf("rationalize: %s\n", p)
	p, _ = CompareJSON([]byte(`{"a":{"b":1}}`), []byte(`{"a":{"b":3,"c":4}}`), Rationalize())
	fmt.Printf("rationalize2: %s\n", p)

	// invalid input -> error layering
	_, e := CompareJSON([]byte(`{bad`), []byte(`{}`))
	fmt.Printf("invalid src json err: %v\n", e)
	_, e = Compare(map[string]any{"a": make(chan int)}, map[string]any{"a": 1.0})
	fmt.Printf("marshal-fail err: %v\n", e)

	// object keys with slash/tilde for ordering
	p, _ = CompareJSON([]byte(`{"a/b":1,"a~b":2,"a":3}`), []byte(`{"a/b":9,"a~b":2,"a":3}`))
	fmt.Printf("escaped keys: %s\n", p)
	b, _ := json.Marshal(p)
	fmt.Printf("marshaled: %s\n", b)
}
