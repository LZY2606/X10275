package jsondiff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func useNumberUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}

func TestScratchExp2(t *testing.T) {
	// 1. test op existence-only check in internal applier
	bogus := Patch{{Type: OperationTest, Path: "/a", Value: float64(99)}}
	out, err := bogus.apply([]byte(`{"a":1}`), false)
	fmt.Printf("test-wrong-value: err=%v out=%s\n", err, out)

	// 2. json.Number changes op set for 1 vs 1.0
	p, err := CompareJSON([]byte(`1`), []byte(`1.0`), UnmarshalFunc(useNumberUnmarshal))
	fmt.Printf("num-usenumber: err=%v patch=%s\n", err, p)

	// 3. reuse Differ without reset -> accumulation?
	d := &Differ{}
	d.Compare(map[string]any{"a": 1.0}, map[string]any{"a": 2.0})
	fmt.Printf("after first: %s\n", d.Patch())
	d.Compare(map[string]any{"x": 1.0}, map[string]any{"x": 2.0})
	fmt.Printf("after second (no reset): %s hashmap=%v ptr=%q\n", d.Patch(), d.hashmap, d.ptr.buf)

	// 4. failed compareWithoutMarshal leaves partial patch
	d2 := &Differ{}
	func() {
		defer func() {
			fmt.Printf("panic recovered: %v\n", recover())
		}()
		d2.Compare(map[string]any{"a": 1.0}, map[string]any{"a": 2.0, "bad": int64(3)})
	}()
	fmt.Printf("after fail: patch=%s hashmap=%v ptr=%q\n", d2.Patch(), d2.hashmap, d2.ptr.buf)
	d2.Reset()
	d2.Compare(map[string]any{"a": 1.0}, map[string]any{"a": 2.0})
	fmt.Printf("after reset+ok: patch=%s hashmap=%v ptr=%q\n", d2.Patch(), d2.hashmap, d2.ptr.buf)

	// 5. fail-once marshal via public Compare
	var calls int
	failOnce := func(v any) ([]byte, error) {
		calls++
		if calls == 2 {
			return nil, fmt.Errorf("synthetic transient marshal failure")
		}
		return json.Marshal(v)
	}
	_, e1 := Compare(map[string]any{"a": 1.0}, map[string]any{"a": 2.0}, MarshalFunc(failOnce))
	p2, e2 := Compare(map[string]any{"a": 1.0}, map[string]any{"a": 2.0}, MarshalFunc(failOnce))
	fmt.Printf("fail-once: e1=%v e2=%v p2=%s calls=%d\n", e1, e2, p2, calls)

	// 6. factorize hashmap leak across failed prepare/diff
	d3 := &Differ{}
	d3.WithOpts(Factorize())
	d3.Compare([]any{map[string]any{"k": "v"}}, []any{map[string]any{"k": "v"}})
	fmt.Printf("d3 hashmap after success: %v\n", d3.hashmap)
}
