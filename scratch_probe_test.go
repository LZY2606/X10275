package jsondiff

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestScratchProbe3(t *testing.T) {
	d := &Differ{}
	after, _ := json.Marshal(map[string]any{"a": 2, "b": 3})
	d.targetBytes = after
	d.WithOpts(Factorize())
	d.Compare(map[string]any{"a": 1}, map[string]any{"a": 2, "b": 3})
	fmt.Println("after first:", len(d.patch), len(d.hashmap))

	// mid-flight panic: invalid json type during recursion
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Println("recovered:", r)
			}
		}()
		d.Compare(map[string]any{"k": make(chan int)}, map[string]any{"k": make(chan int)})
	}()
	fmt.Println("after panic: patchLen=", len(d.patch), "hashmap=", len(d.hashmap))

	d.Reset()
	fmt.Println("after reset: patchLen=", len(d.patch), "hashmap=", len(d.hashmap), "ptrLen=", len(d.ptr.buf), "snap=", d.snapshotPatchLen)

	after2, _ := json.Marshal(map[string]any{"a": 9})
	d.targetBytes = after2
	d.Compare(map[string]any{"a": 1}, map[string]any{"a": 9})
	fmt.Println("retry patch:", d.patch.String())

	// no reset => accumulation across comparisons (documented behavior)
	d2 := &Differ{}
	d2.targetBytes, _ = json.Marshal(2)
	d2.Compare(1, 2)
	d2.targetBytes, _ = json.Marshal(4)
	d2.Compare(3, 4)
	fmt.Println("no-reset accumulated ops:", len(d2.patch))
}
