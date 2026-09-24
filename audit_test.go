package jsondiff

// Regression material for the audit "jsondiff: equivalence from node
// comparison to patch application". See ANALYSIS.md for the full audit.
//
// The tests below only use the package's public entry points
// (Compare/CompareJSON/CompareWithoutMarshal) plus the internal test-only
// Patch.apply, and never fabricate internal final objects.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func auditMustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %s", err)
	}
	return b
}

// applyRoundTrip applies p to the JSON encoding of src and verifies that
// the result is semantically equal to the JSON encoding of tgt. It reports
// the first diverging position with context instead of a bare boolean.
func applyRoundTrip(t *testing.T, src, tgt any, p Patch) {
	t.Helper()
	out, err := p.apply(auditMustJSON(t, src), false)
	if err != nil {
		t.Fatalf("apply failed: %s\npatch:\n%s", err, p)
	}
	var got, want any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("applied document is not valid JSON: %s\nout: %s", err, out)
	}
	if err := json.Unmarshal(auditMustJSON(t, tgt), &want); err != nil {
		t.Fatalf("target is not valid JSON: %s", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip violation: apply(diff(a,b),a) != b\n got: %s\nwant: %s\npatch:\n%s",
			auditMustJSON(t, got), auditMustJSON(t, want), p)
	}
}

// assertOpSequence compares got against the expected operations and
// reports the first mismatching index with full context.
func assertOpSequence(t *testing.T, got Patch, want []Operation) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("op count mismatch: got %d, want %d\ngot patch:\n%s", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Type != w.Type || g.Path != w.Path || g.From != w.From {
			t.Fatalf("first violation at op #%d:\n got: %s\nwant: %s\nfull patch:\n%s",
				i, g, w, got)
		}
	}
}

func patchFingerprint(p Patch) string {
	var sb strings.Builder
	for i, op := range p {
		if i != 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "%s %s <- %s", op.Type, op.Path, op.From)
	}
	return sb.String()
}

// TestAudit_RoundTrip is the fixed-input group: each case is a minimal
// input exercising one pipeline stage, verifying both the operation
// sequence (with order and source path) and apply(diff(a,b),a)==b.
func TestAudit_RoundTrip(t *testing.T) {
	useNumber := func(b []byte, v any) error {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		return dec.Decode(v)
	}

	t.Run("duplicate_array_elements", func(t *testing.T) {
		src := []any{1.0, 1.0, 2.0}
		tgt := []any{1.0, 2.0, 2.0}
		p, err := Compare(src, tgt)
		if err != nil {
			t.Fatal(err)
		}
		assertOpSequence(t, p, []Operation{
			{Type: OperationReplace, Path: "/1"},
		})
		applyRoundTrip(t, src, tgt, p)
	})

	t.Run("duplicate_array_elements_equivalent_risk", func(t *testing.T) {
		// Risk A (ANALYSIS.md §2): Equivalent() treats the multisets
		// [1,1,2] and [1,2,2] as equal sets and emits an empty patch,
		// which does NOT satisfy the round-trip invariant.
		src := []any{1.0, 1.0, 2.0}
		tgt := []any{1.0, 2.0, 2.0}
		p, err := Compare(src, tgt, Equivalent())
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 0 {
			t.Fatalf("expected empty (unsound) patch, got:\n%s", p)
		}
		out, err := p.apply(auditMustJSON(t, src), false)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(out, auditMustJSON(t, tgt)) {
			t.Errorf("risk A disappeared: empty patch unexpectedly produced target")
		}
		t.Logf("documented risk A: %s applied to %s stays %s", p,
			auditMustJSON(t, src), out)
	})

	t.Run("number_representation_normalized", func(t *testing.T) {
		// Normalize flattens 1.0 and 1 to the same float64: empty patch.
		p, err := CompareJSON([]byte(`{"n":1.0}`), []byte(`{"n":1}`))
		if err != nil {
			t.Fatal(err)
		}
		assertOpSequence(t, p, nil)
		applyRoundTrip(t, map[string]any{"n": 1.0}, map[string]any{"n": 1.0}, p)
	})

	t.Run("number_representation_json_number", func(t *testing.T) {
		// Custom unmarshal changes normalization: lexical json.Number
		// comparison makes 1.0 vs 1 a real replace operation.
		p, err := CompareJSON([]byte(`{"n":1.0}`), []byte(`{"n":1}`),
			UnmarshalFunc(useNumber))
		if err != nil {
			t.Fatal(err)
		}
		assertOpSequence(t, p, []Operation{
			{Type: OperationReplace, Path: "/n"},
		})
		if g, w := fmt.Sprint(p[0].Value), "1"; g != w {
			t.Fatalf("first violation at op #0 value: got %q, want %q", g, w)
		}
		// The target is already lexical JSON, so verify against raw bytes.
		out, err := p.apply([]byte(`{"n":1.0}`), false)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, []byte(`{"n":1}`)) {
			t.Fatalf("round-trip violation: got %s, want %s", out, `{"n":1}`)
		}
	})

	t.Run("object_key_reorder", func(t *testing.T) {
		src := map[string]any{"a": 2.0, "b": 1.0}
		tgt := map[string]any{"b": 1.0, "a": 2.0}
		p, err := Compare(src, tgt)
		if err != nil {
			t.Fatal(err)
		}
		assertOpSequence(t, p, nil)
		applyRoundTrip(t, src, tgt, p)
	})

	t.Run("root_replacement_type_change", func(t *testing.T) {
		// Incomparable types at the root produce an add operation on "".
		src := []any{1.0}
		tgt := map[string]any{"a": 1.0}
		p, err := Compare(src, tgt)
		if err != nil {
			t.Fatal(err)
		}
		assertOpSequence(t, p, []Operation{
			{Type: OperationAdd, Path: ""},
		})
		applyRoundTrip(t, src, tgt, p)
	})

	t.Run("root_scalar_invertible", func(t *testing.T) {
		// Invertible prepends a test operation to the root replace.
		src := 1.0
		tgt := 2.0
		p, err := Compare(src, tgt, Invertible())
		if err != nil {
			t.Fatal(err)
		}
		assertOpSequence(t, p, []Operation{
			{Type: OperationTest, Path: ""},
			{Type: OperationReplace, Path: ""},
		})
		applyRoundTrip(t, src, tgt, p)

		// The test operation carries the old value, which is what Invert
		// consumes to rebuild the original document.
		ip, err := p.Invert()
		if err != nil {
			t.Fatal(err)
		}
		applyRoundTrip(t, tgt, src, ip)
	})

	t.Run("move_from_removed", func(t *testing.T) {
		// LCS + Factorize turns remove/add of the same value into moves.
		src := []any{1.0, 2.0, 3.0}
		tgt := []any{3.0, 2.0, 1.0}
		p, err := Compare(src, tgt, LCS(), Factorize())
		if err != nil {
			t.Fatal(err)
		}
		assertOpSequence(t, p, []Operation{
			{Type: OperationMove, Path: "/2", From: "/0"},
			{Type: OperationMove, Path: "/1", From: "/0"},
		})
		applyRoundTrip(t, src, tgt, p)
	})

	t.Run("copy_from_unchanged", func(t *testing.T) {
		// Factorize reuses an unchanged node with a copy operation.
		src := map[string]any{"a": map[string]any{"x": 1.0}}
		tgt := map[string]any{
			"a": map[string]any{"x": 1.0},
			"b": map[string]any{"x": 1.0},
		}
		p, err := Compare(src, tgt, Factorize())
		if err != nil {
			t.Fatal(err)
		}
		assertOpSequence(t, p, []Operation{
			{Type: OperationCopy, Path: "/b", From: "/a"},
		})
		applyRoundTrip(t, src, tgt, p)

		// Copy operations are explicitly not invertible.
		if _, err := p.Invert(); err != ErrAmbiguousCopyOp {
			t.Fatalf("expected ErrAmbiguousCopyOp, got %v", err)
		}
	})

	t.Run("invertible_no_copy", func(t *testing.T) {
		// With Invertible, factorize must not emit copy: it falls back
		// to an add so the patch remains reversible.
		src := map[string]any{"a": map[string]any{"x": 1.0}}
		tgt := map[string]any{
			"a": map[string]any{"x": 1.0},
			"b": map[string]any{"x": 1.0},
		}
		p, err := Compare(src, tgt, Factorize(), Invertible())
		if err != nil {
			t.Fatal(err)
		}
		assertOpSequence(t, p, []Operation{
			{Type: OperationAdd, Path: "/b"},
		})
		applyRoundTrip(t, src, tgt, p)
	})

	t.Run("rationalize_collapses_operations", func(t *testing.T) {
		// Several nested edits are more expensive than one replace:
		// rationalize must collapse them, proving the shortest patch
		// is not the only correct one (this one is longer than 3,
		// but still satisfies the invariant).
		src := map[string]any{"o": map[string]any{"a": 1.0, "b": 2.0}}
		tgt := map[string]any{"o": map[string]any{"a": 9.0, "b": 8.0}}
		p, err := Compare(src, tgt, Rationalize())
		if err != nil {
			t.Fatal(err)
		}
		assertOpSequence(t, p, []Operation{
			{Type: OperationReplace, Path: "/o"},
		})
		applyRoundTrip(t, src, tgt, p)
	})
}

// TestAudit_SideEffectOnInput pins risk B (ANALYSIS.md §2): when the
// entry skips normalization, Equivalent() sorts the caller's NESTED
// arrays in place via the hasher. The digest order depends on the
// process-local maphash seed, so the test drives the same production
// path (Differ.Compare) with the Differ's own hasher seed to build an
// input whose order is guaranteed to be changed. The normalizing
// public entry Compare must stay immune.
func TestAudit_SideEffectOnInput(t *testing.T) {
	values := []float64{0, 1, 2, 3, 4, 5}

	// d is the Differ under test; its hasher is lazily seeded on first
	// use. Force the seed and read it back, exactly like sortArray does
	// (hash.go: h1.mh.SetSeed(h.mh.Seed())), so the expected digest
	// order below is computed with the very same seed.
	d := &Differ{}
	d.applyOpts(Equivalent())
	d.hasher.digest(0.0, false)
	seeded := hasher{}
	seeded.mh.SetSeed(d.hasher.mh.Seed())

	digests := map[float64]uint64{}
	for _, v := range values {
		digests[v] = seeded.digest(v, true)
	}
	digestOrdered := make([]float64, len(values))
	copy(digestOrdered, values)
	for i := 0; i < len(digestOrdered); i++ {
		for j := i + 1; j < len(digestOrdered); j++ {
			if digests[digestOrdered[j]] < digests[digestOrdered[i]] {
				digestOrdered[i], digestOrdered[j] = digestOrdered[j], digestOrdered[i]
			}
		}
	}
	// Reverse the digest order: a permutation guaranteed to differ from
	// it, since all digests are distinct.
	reversed := make([]any, len(digestOrdered))
	wantOrdered := make([]any, len(digestOrdered))
	for i, v := range digestOrdered {
		reversed[len(reversed)-1-i] = v
		wantOrdered[i] = v
	}
	original := append([]any{}, reversed...)

	// Two outer arrays are "unordered equal"; the nested slice is the
	// caller's own value, shared by reference.
	src := []any{reversed, []any{9.0}}
	tgt := []any{[]any{9.0}, append([]any{}, wantOrdered...)}
	d.Compare(src, tgt)

	if !reflect.DeepEqual(reversed, wantOrdered) {
		t.Errorf("documented risk B: caller's nested array mutated in place: got %v, want %v",
			reversed, wantOrdered)
	}
	if reflect.DeepEqual(reversed, original) {
		t.Errorf("risk B no longer reproduces: nested array left untouched")
	}
	t.Logf("documented risk B: caller nested array %v reordered to %v", original, reversed)

	// The normalizing entry marshals/unmarshals first, so the caller's
	// own slice must be left untouched.
	private := append([]any{}, original...)
	if _, err := Compare([]any{private, []any{9.0}},
		[]any{[]any{9.0}, append([]any{}, wantOrdered...)}, Equivalent()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(private, original) {
		t.Errorf("Compare must not mutate caller data: got %v, want %v", private, original)
	}
}

// TestAudit_TestOpValueNotChecked pins risk C (ANALYSIS.md §2): apply
// validates only the existence of a test path, never its value.
func TestAudit_TestOpValueNotChecked(t *testing.T) {
	bogus := Patch{{Type: OperationTest, Path: "/n", Value: 999.0}}
	out, err := bogus.apply([]byte(`{"n":1}`), false)
	if err != nil {
		t.Errorf("risk C disappeared: apply now rejects wrong-value test: %s", err)
	}
	if string(out) != `{"n":1}` {
		t.Errorf("unexpected apply output: %s", out)
	}
	// A test on a missing path must still fail (existence is checked).
	missing := Patch{{Type: OperationTest, Path: "/absent", Value: 1.0}}
	if _, err := missing.apply([]byte(`{"n":1}`), false); err == nil {
		t.Errorf("expected error for test on missing path")
	}
}

// TestAudit_FactorizeCopySourceNondeterminism documents ANALYSIS.md §4:
// with duplicate equal subtrees, prepare's Go map iteration makes the
// copy source vary across runs. Both variants are semantically correct;
// the test asserts membership in the equivalence set, never one answer.
func TestAudit_FactorizeCopySourceNondeterminism(t *testing.T) {
	src := map[string]any{
		"a": map[string]any{"x": 1.0},
		"b": map[string]any{"x": 1.0},
	}
	tgt := map[string]any{
		"a": map[string]any{"x": 1.0},
		"b": map[string]any{"x": 1.0},
		"c": map[string]any{"x": 1.0},
	}
	allowed := map[string]bool{
		"copy /c <- /a": true,
		"copy /c <- /b": true,
	}
	seen := map[string]int{}
	for i := 0; i < 100; i++ {
		p, err := Compare(src, tgt, Factorize())
		if err != nil {
			t.Fatal(err)
		}
		fp := patchFingerprint(p)
		if !allowed[fp] {
			t.Fatalf("first violation at run %d: unexpected patch %q", i, fp)
		}
		applyRoundTrip(t, src, tgt, p)
		seen[fp]++
	}
	t.Logf("observed equivalent patches over 100 runs: %v", seen)
}

// TestAudit_OrderStability is the second input group: it changes only
// result-irrelevant enumeration and scheduling conditions (map insertion
// order, goroutine interleaving) and proves the operation sequence and
// its order remain stable.
func TestAudit_OrderStability(t *testing.T) {
	src := map[string]any{"k1": 1.0, "k2": []any{1.0, 2.0}, "k3": map[string]any{"n": 5.0}}
	tgt := map[string]any{"k2": []any{2.0, 1.0}, "k3": map[string]any{"n": 6.0}, "k4": true}

	// Reference fingerprint from a single run.
	ref, err := Compare(src, tgt, LCS(), Factorize(), Invertible())
	if err != nil {
		t.Fatal(err)
	}
	want := patchFingerprint(ref)

	// Same semantic input, different map insertion order.
	rebuilt := map[string]any{}
	for _, k := range []string{"k3", "k1", "k2"} {
		rebuilt[k] = src[k]
	}
	p, err := Compare(rebuilt, tgt, LCS(), Factorize(), Invertible())
	if err != nil {
		t.Fatal(err)
	}
	if got := patchFingerprint(p); got != want {
		t.Fatalf("first violation: insertion order changed the patch\n got: %q\nwant: %q", got, want)
	}

	// Same input compared concurrently from many goroutines released by
	// a barrier; every result must match the reference exactly.
	const workers = 16
	var barrier sync.WaitGroup
	barrier.Add(workers)
	results := make([]string, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			barrier.Done()
			barrier.Wait() // all goroutines start together
			p, err := Compare(src, tgt, LCS(), Factorize(), Invertible())
			if err != nil {
				t.Errorf("worker %d: %s", w, err)
				return
			}
			results[w] = patchFingerprint(p)
		}(w)
	}
	wg.Wait()
	for w, got := range results {
		if got != want {
			t.Errorf("first violation at worker %d:\n got: %q\nwant: %q", w, got, want)
		}
	}
	applyRoundTrip(t, src, tgt, ref)
}

// auditBadType is not part of the JSON type set and triggers the
// recoverable invalidJSONTypeError panic inside a comparison.
type auditBadType struct{}

// TestAudit_RecoverableFailure injects a recoverable failure in the
// middle of a comparison and verifies the recovery protocol of
// ANALYSIS.md §5: the public entry reports a typed error, a reused
// Differ keeps partial state until Reset, and a retry after Reset
// produces exactly the same result as a fresh Differ — no cache,
// counter or temporary state from the failed call leaks through.
func TestAudit_RecoverableFailure(t *testing.T) {
	src := map[string]any{"a": 1.0, "z": auditBadType{}}
	tgt := map[string]any{"a": 2.0, "z": auditBadType{}}

	// 1. Public entry: the failure surfaces through the existing error
	//    layer, and is repeatable for the same input.
	for i := 0; i < 2; i++ {
		p, err := CompareWithoutMarshal(src, tgt)
		if err == nil {
			t.Fatal("expected error for invalid JSON type")
		}
		if p != nil {
			t.Fatalf("expected nil patch on failure, got:\n%s", p)
		}
		want := "jsondiff: invalid json type: jsondiff.auditBadType"
		if err.Error() != want {
			t.Fatalf("error outside existing layer: got %q, want %q", err, want)
		}
	}

	// 2. A Differ reused across the failure keeps partial operations
	//    (the replace for key "a" was emitted before the panic on "z").
	d := &Differ{}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected panic from invalid JSON type")
			}
		}()
		d.Compare(src, tgt)
	}()
	if len(d.Patch()) == 0 {
		t.Fatal("expected partial operations to remain after mid-compare failure")
	}
	t.Logf("partial state after failure: %s", d.Patch())

	// 3. Retrying without Reset would leak the stale operations into
	//    the next result; Reset is the documented recovery step.
	d.Reset()
	d.Compare(map[string]any{"k": 1.0}, map[string]any{"k": 2.0})
	recovered := d.Patch()

	fresh, err := Compare(map[string]any{"k": 1.0}, map[string]any{"k": 2.0})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := patchFingerprint(recovered), patchFingerprint(fresh); got != want {
		t.Fatalf("recovered result carries failed-call state:\n got: %q\nwant: %q", got, want)
	}
	assertOpSequence(t, recovered, []Operation{
		{Type: OperationReplace, Path: "/k"},
	})
	applyRoundTrip(t, map[string]any{"k": 1.0}, map[string]any{"k": 2.0}, recovered)
}

// TestAudit_FuzzRoundTrip proves the invariant on generated inputs with
// a fixed seed. On failure it prints a minimized reproducer.
func TestAudit_FuzzRoundTrip(t *testing.T) {
	const seed = 20260919
	rng := rand.New(rand.NewSource(seed))

	optionSets := [][]Option{
		nil,
		{LCS()},
		{LCS(), Factorize()},
		{Factorize(), Invertible()},
		{Rationalize()},
	}
	for i := 0; i < 200; i++ {
		a := genValue(rng, 0)
		b := genValue(rng, 0)
		opts := optionSets[rng.Intn(len(optionSets))]

		p, err := Compare(a, b, opts...)
		if err != nil {
			t.Fatalf("seed=%d case=%d compare failed: %s\na=%s\nb=%s",
				seed, i, err, auditMustJSON(t, a), auditMustJSON(t, b))
		}
		out, err := p.apply(auditMustJSON(t, a), false)
		if err != nil {
			na, nb := minimizePair(t, a, b, opts)
			t.Fatalf("seed=%d case=%d apply failed: %s\nminimal a=%s\nminimal b=%s\npatch:\n%s",
				seed, i, err, auditMustJSON(t, na), auditMustJSON(t, nb), p)
		}
		var got, want any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("seed=%d case=%d invalid output: %s", seed, i, err)
		}
		if err := json.Unmarshal(auditMustJSON(t, b), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			na, nb := minimizePair(t, a, b, opts)
			t.Fatalf("seed=%d case=%d round-trip violation\nminimal a=%s\nminimal b=%s\ngot=%s\npatch:\n%s",
				seed, i, auditMustJSON(t, na), auditMustJSON(t, nb), out, p)
		}
	}
}

// genValue generates a random JSON-compatible value; duplicates are
// frequent by construction (small alphabets).
func genValue(rng *rand.Rand, depth int) any {
	switch rng.Intn(8) {
	case 0:
		return nil
	case 1:
		return rng.Intn(2) == 0
	case 2, 3:
		return float64(rng.Intn(4))
	case 4:
		return []string{"a", "b", "c"}[rng.Intn(3)]
	case 5, 6:
		n := rng.Intn(4)
		arr := make([]any, n)
		for i := range arr {
			if depth < 3 {
				arr[i] = genValue(rng, depth+1)
			} else {
				arr[i] = float64(rng.Intn(4))
			}
		}
		return arr
	default:
		n := rng.Intn(4)
		obj := make(map[string]any, n)
		for i := 0; i < n; i++ {
			k := fmt.Sprintf("k%d", rng.Intn(4))
			if depth < 3 {
				obj[k] = genValue(rng, depth+1)
			} else {
				obj[k] = float64(rng.Intn(4))
			}
		}
		return obj
	}
}

// minimizePair shrinks a failing (a, b) pair by dropping top-level
// elements and keys while the round-trip still fails, so a reported
// failure prints the smallest reproducer found.
func minimizePair(t *testing.T, a, b any, opts []Option) (any, any) {
	t.Helper()
	fails := func(x, y any) bool {
		p, err := Compare(x, y, opts...)
		if err != nil {
			return false
		}
		out, err := p.apply(auditMustJSON(t, x), false)
		if err != nil {
			return true
		}
		var got, want any
		if json.Unmarshal(out, &got) != nil ||
			json.Unmarshal(auditMustJSON(t, y), &want) != nil {
			return false
		}
		return !reflect.DeepEqual(got, want)
	}
	// Try dropping one top-level item from either side at a time.
	changed := true
	for changed {
		changed = false
		if arr, ok := a.([]any); ok {
			for i := 0; i < len(arr); i++ {
				cand := append(append([]any{}, arr[:i]...), arr[i+1:]...)
				if fails(cand, b) {
					a, changed = cand, true
					break
				}
			}
		}
		if arr, ok := b.([]any); ok {
			for i := 0; i < len(arr); i++ {
				cand := append(append([]any{}, arr[:i]...), arr[i+1:]...)
				if fails(a, cand) {
					b, changed = cand, true
					break
				}
			}
		}
		if obj, ok := a.(map[string]any); ok {
			for k := range obj {
				cand := map[string]any{}
				for kk, vv := range obj {
					if kk != k {
						cand[kk] = vv
					}
				}
				if fails(cand, b) {
					a, changed = cand, true
					break
				}
			}
		}
		if obj, ok := b.(map[string]any); ok {
			for k := range obj {
				cand := map[string]any{}
				for kk, vv := range obj {
					if kk != k {
						cand[kk] = vv
					}
				}
				if fails(a, cand) {
					b, changed = cand, true
					break
				}
			}
		}
	}
	return a, b
}
