package jsondiff

import (
	"fmt"
	"testing"
)

func TestScratchNorm(t *testing.T) {
	p1, e1 := Compare(1, 1.0)
	fmt.Printf("Compare(1,1.0): %s err=%v\n", p1.String(), e1)
	p2, e2 := CompareWithoutMarshal(1, 1.0)
	fmt.Printf("CompareWithoutMarshal(1,1.0): %s err=%v\n", p2.String(), e2)
}
