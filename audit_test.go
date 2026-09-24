package jsondiff

// Regression material dedicated to the equivalence audit
// "jsondiff: from node comparison to patch application".
//
// Every case enters the pipeline through a documented public
// entry point (Compare, CompareJSON or CompareWithoutMarshal)
// and verifies semantics by applying the generated patch with
// the production applier used by the rest of the test suite:
//
//	input normalize -> node comparison (equal.go)
//	-> array/object algorithm (compareArrays/compareArraysLCS/
//	   compareObjects) -> operation ordering -> Patch.apply.
//
// No test forges internal terminal objects: patches are only
// obtained from a real Differ run, and Patch.apply is the same
// unexported production method exercised by runTestCase.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// opSig is the stable, order-sensitive signature of a
// generated operation. Signatures are compared as slices
// so that a failure reports the first violating position.
type opSig struct {
	typ  string
	path string
	from string
}

func signature(p Patch) []opSig {
	s := make([]opSig, len(p))
	for i, op := range p {
		s[i] = opSig{typ: op.Type, path: op.Path, from: op.From}
	}
	return s
}

func firstSigMismatch(got, want []opSig) int {
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			return i
		}
	}
	if len(got) != len(want) {
		return n
	}
	return -1
}

func compactJSON(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatalf("invalid JSON in test material: %v: %s", err, b)
	}
	return buf.Bytes()
}

// roundTrip applies patch to src with the production applier
// and returns whether the result is byte-equal to the compacted
// target. On failure it reports the first violating operation
// (the prefix length at which the document first diverges) and
// the surrounding documents instead of a bare boolean.
func roundTrip(t *testing.T, p Patch, src, tgt []byte) (bool, string) {
	t.Helper()
	want := compactJSON(t, tgt)

	got, err := p.apply(src, false)
	if err != nil {
		return false, fmt.Sprintf("apply error: %v", err)
	}
	got = compactJSON(t, got)
	if bytes.Equal(got, want) {
		return true, ""
	}
	// Attribute the divergence: report the first operation
	// whose prefix application errors. Intermediate documents
	// are never compared to the final target (they are not
	// expected to equal it); when every prefix applies, the
	// final operation is the first position where the full
	// patch fails to produce the target.
	badOp := -1
	for i := 1; i <= len(p); i++ {
		if _, err := p[:i].apply(src, false); err != nil {
			badOp = i - 1
			break
		}
	}
	if badOp == -1 && len(p) > 0 {
		badOp = len(p) - 1
	}
	ctx := ""
	if badOp >= 0 {
		ctx = fmt.Sprintf("first violating op #%d: %s; ", badOp, p[badOp])
	}
	return false, fmt.Sprintf("%sgot %s, want %s (patch length %d)", ctx, got, want, len(p))
}

// runAuditCase drives one fixed input through CompareJSON (or
// Compare) and verifies both the caller-visible operation set,
// in order, and the round-trip invariant.
func runAuditCase(t *testing.T, tc auditCase) {
	t.Helper()

	patch, err := tc.diff()
	if err != nil {
		t.Fatalf("Compare returned error %v", err)
	}
	if i := firstSigMismatch(signature(patch), tc.wantOps); i != -1 {
		t.Errorf("operation order mismatch at #%d:\n  got  %v\n  want %v\n  full got:  %s",
			i, sigOrNil(signature(patch), i), sigOrNil(tc.wantOps, i), patch.String())
		return
	}
	for i, op := range patch {
		if i >= len(tc.wantValues) {
			break
		}
		want := tc.wantValues[i]
		if want == nil {
			continue
		}
		if !reflect.DeepEqual(op.Value, want) {
			t.Errorf("op #%d (%s) value mismatch: got %#v (%T), want %#v (%T)",
				i, op.Type, op.Value, op.Value, want, want)
		}
	}

	src := compactJSON(t, tc.src)
	srcBefore := bytes.Clone(src)
	if ok, msg := roundTrip(t, patch, src, tc.tgt); !ok {
		t.Errorf("round-trip apply(diff(a,b),a) != b: %s", msg)
	}
	if !bytes.Equal(src, srcBefore) {
		t.Errorf("applier mutated the caller source document:\n before %s\n after  %s", srcBefore, src)
	}
	// Determinism: a second independent run through the same
	// public entry must produce the identical ordered patch.
	patch2, err := tc.diff()
	if err != nil {
		t.Fatalf("second Compare returned error %v", err)
	}
	if patch.String() != patch2.String() {
		t.Errorf("non-deterministic patch:\n run1 %s\n run2 %s", patch, patch2)
	}
}

func sigOrNil(sigs []opSig, i int) opSig {
	if i < len(sigs) {
		return sigs[i]
	}
	return opSig{"<missing>", "", ""}
}

type auditCase struct {
	name       string
	src, tgt   []byte
	opts       []Option
	wantOps    []opSig
	wantValues []any
	diff       func() (Patch, error)
}

// jsonCase builds an audit case driven through CompareJSON.
func jsonCase(name, src, tgt string, opts []Option, ops []opSig, values []any) auditCase {
	return auditCase{
		name:       name,
		src:        []byte(src),
		tgt:        []byte(tgt),
		opts:       opts,
		wantOps:    ops,
		wantValues: values,
		diff: func() (Patch, error) {
			return CompareJSON([]byte(src), []byte(tgt), opts...)
		},
	}
}

// TestAuditFixedRoundTrip is the fixed-input group required by
// the equivalence audit. Each case uses a minimal input to
// trigger one node-comparison/array-algorithm branch and then
// checks the ordered operations, their values and the
// apply(diff(a,b),a)=b invariant.
func TestAuditFixedRoundTrip(t *testing.T) {
	s := func(v ...opSig) []opSig { return v }
	vs := func(v ...any) []any { return v }

	cases := []auditCase{
		// --- duplicate array elements ---
		jsonCase("dup_elements_default_indexwise",
			`["x","x",1]`, `["x",1,"x"]`, nil,
			s(
				opSig{OperationReplace, "/1", ""},
				opSig{OperationReplace, "/2", ""},
			),
			vs(float64(1), "x"),
		),
		jsonCase("dup_elements_lcs_remove_add",
			`["x","x",1]`, `["x",1,"x"]`, []Option{LCS()},
			s(
				opSig{OperationRemove, "/0", ""},
				opSig{OperationAdd, "/2", ""},
			),
			vs(nil, "x"),
		),
		jsonCase("dup_elements_lcs_factorize_move",
			`["x","x",1]`, `["x",1,"x"]`, []Option{LCS(), Factorize()},
			s(opSig{OperationMove, "/2", "/0"}),
			nil,
		),
		jsonCase("dup_shrink_lcs_factorize_ambiguous_remove",
			`["x","x"]`, `["x"]`, []Option{LCS(), Factorize()},
			s(opSig{OperationRemove, "/0", ""}),
			nil,
		),
		// The default index-wise algorithm is also valid here:
		// the shortest patch is not treated as the only answer.
		jsonCase("dup_shrink_default_is_equally_valid",
			`["x","x"]`, `["x"]`, nil,
			s(opSig{OperationRemove, "/1", ""}),
			nil,
		),

		// --- number representation ---
		jsonCase("number_representation_normalizes_equal",
			`{"n":1.0}`, `{"n":1}`, nil,
			s(),
			nil,
		),
		jsonCase("number_value_replace",
			`{"n":1}`, `{"n":2}`, nil,
			s(opSig{OperationReplace, "/n", ""}),
			vs(float64(2)),
		),
		// --- object key reordering ---
		jsonCase("object_key_reorder_noop",
			`{"b":1,"a":2}`, `{"a":2,"b":1}`, nil,
			s(),
			nil,
		),
		jsonCase("object_multi_change_sorted_key_order",
			`{"b":1,"a":2,"z":3}`, `{"b":9,"a":2,"z":4}`, nil,
			s(
				opSig{OperationReplace, "/b", ""},
				opSig{OperationReplace, "/z", ""},
			),
			vs(float64(9), float64(4)),
		),

		// --- root replacement ---
		jsonCase("root_array_to_object_add_at_root",
			`[1]`, `{"a":1}`, nil,
			s(opSig{OperationAdd, "", ""}),
			vs(map[string]any{"a": float64(1)}),
		),
		jsonCase("root_scalar_type_change_add_at_root",
			`1`, `"x"`, nil,
			s(opSig{OperationAdd, "", ""}),
			vs("x"),
		),

		// --- move / copy via factorization ---
		jsonCase("factorized_removed_then_added_becomes_move",
			`{"a":{"k":"v"}}`, `{"b":{"k":"v"}}`, []Option{Factorize()},
			s(opSig{OperationMove, "/b", "/a"}),
			nil,
		),
		jsonCase("factorized_unchanged_value_becomes_copy",
			`{"a":{"k":"v"},"c":1}`,
			`{"a":{"k":"v"},"b":{"k":"v"},"c":1}`,
			[]Option{Factorize()},
			s(opSig{OperationCopy, "/b", "/a"}),
			nil,
		),
		jsonCase("invertible_disables_copy_uses_add",
			`{"a":{"k":"v"},"c":1}`,
			`{"a":{"k":"v"},"b":{"k":"v"},"c":1}`,
			[]Option{Factorize(), Invertible()},
			s(opSig{OperationAdd, "/b", ""}),
			vs(map[string]any{"k": "v"}),
		),

		// --- test operations (invertible) ---
		jsonCase("invertible_replace_preceded_by_test",
			`{"a":1}`, `{"a":2}`, []Option{Invertible()},
			s(
				opSig{OperationTest, "/a", ""},
				opSig{OperationReplace, "/a", ""},
			),
			vs(float64(1), float64(2)),
		),
		jsonCase("invertible_remove_preceded_by_test",
			`{"a":1}`, `{"b":1}`, []Option{Invertible()},
			s(
				opSig{OperationTest, "/a", ""},
				opSig{OperationRemove, "/a", ""},
				opSig{OperationAdd, "/b", ""},
			),
			vs(float64(1), nil, float64(1)),
		),

		// --- rationalize changes the operation set ---
		jsonCase("rationalize_collapses_to_root_replace",
			`{"a":1,"b":2,"c":3}`, `{"a":9}`, []Option{Rationalize()},
			s(opSig{OperationReplace, "", ""}),
			vs(map[string]any{"a": float64(9)}),
		),
	}

	// --- custom marshal changes the normalized input ---
	cases = append(cases, auditCase{
		name: "custom_marshal_func_changes_operation_set",
		src:  []byte(`{"y":1}`),
		tgt:  []byte(`{"y":2}`),
		wantOps: s(
			opSig{OperationReplace, "/y", ""},
		),
		wantValues: vs(float64(2)),
		diff: func() (Patch, error) {
			type widget struct {
				X int `json:"x"`
				Y int `json:"y"`
			}
			// The custom marshal hides field "x" entirely, so
			// the normalized documents only contain "y": the
			// patch must not carry an operation for "/x".
			hideX := MarshalFunc(func(v any) ([]byte, error) {
				if w, ok := v.(widget); ok {
					return json.Marshal(struct {
						Y int `json:"y"`
					}{Y: w.Y})
				}
				return json.Marshal(v)
			})
			return Compare(widget{X: 1, Y: 1}, widget{X: 2, Y: 2}, hideX)
		},
	})
	cases = append(cases, auditCase{
		name:    "custom_marshal_func_makes_change_invisible",
		src:     []byte(`{}`),
		tgt:     []byte(`{}`),
		wantOps: s(),
		diff: func() (Patch, error) {
			type widget struct {
				X int `json:"x"`
			}
			hideAll := MarshalFunc(func(v any) ([]byte, error) {
				if _, ok := v.(widget); ok {
					return []byte(`{}`), nil
				}
				return json.Marshal(v)
			})
			return Compare(widget{X: 1}, widget{X: 2}, hideAll)
		},
	})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runAuditCase(t, tc)
		})
	}
}

// TestAuditKnownRisks documents the three real risk classes
// identified while tracing the pipeline. When an input falls
// into one of them the audit refuses to certify round-trip
// equivalence ("unknown/unsupported") rather than guessing
// success from an empty or cheap patch.
func TestAuditKnownRisks(t *testing.T) {
	t.Run("risk1_false_equivalence_duplicates_unsupported", func(t *testing.T) {
		// Same multiset of values, different multiplicities:
		// unorderedDeepEqualSlice (differ.go:439) stores
		// hashes in a set, so {"x","y"} is accepted for
		// {"x","x","y"} and the patch is empty.
		p, err := CompareJSON([]byte(`["x","x","y"]`), []byte(`["y","x"]`), Equivalent())
		if err != nil {
			t.Fatalf("CompareJSON error: %v", err)
		}
		if len(p) != 0 {
			t.Fatalf("expected empty patch exposing the false-equivalence risk, got:\n%s", p)
		}
		if ok, msg := roundTrip(t, p, []byte(`["x","x","y"]`), []byte(`["y","x"]`)); ok {
			t.Fatal("risk case must not certify: empty patch leaves source unchanged")
		} else if !strings.Contains(msg, `got ["x","x","y"]`) {
			t.Fatalf("failure context must show unpatched document, got: %s", msg)
		}
		// Same input without Equivalent produces a real,
		// applicable patch and round-trips correctly.
		p2, err := CompareJSON([]byte(`["x","x","y"]`), []byte(`["y","x"]`))
		if err != nil {
			t.Fatalf("CompareJSON error: %v", err)
		}
		if ok, _ := roundTrip(t, p2, []byte(`["x","x","y"]`), []byte(`["y","x"]`)); !ok {
			t.Fatalf("default comparison must round-trip, got:\n%s", p2)
		}
	})

	t.Run("risk2_bigint_precision_collision_unsupported", func(t *testing.T) {
		// Both integers round-trip through float64 to the
		// same value (equal.go:85), so Compare (which
		// normalizes via encoding/json) reports no change.
		src := []byte(`{"n":9007199254740993}`)
		tgt := []byte(`{"n":9007199254740992}`)
		p, err := CompareJSON(src, tgt)
		if err != nil {
			t.Fatalf("CompareJSON error: %v", err)
		}
		if len(p) != 0 {
			t.Fatalf("expected empty patch exposing the precision risk, got:\n%s", p)
		}
		if ok, _ := roundTrip(t, p, src, tgt); ok {
			t.Fatal("risk case must not certify: source and target differ textually")
		}
		// CompareWithoutMarshal with json.Number preserves
		// the representation and yields a certifiable patch.
		var si, ti any
		mustUseNumber(t, src, &si)
		mustUseNumber(t, tgt, &ti)
		p2, err := CompareWithoutMarshal(si, ti)
		if err != nil {
			t.Fatalf("CompareWithoutMarshal error: %v", err)
		}
		if len(p2) != 1 || p2[0].Type != OperationReplace || p2[0].Path != "/n" {
			t.Fatalf("expected one replace /n with json.Number, got:\n%s", p2)
		}
		if p2[0].Value != json.Number("9007199254740992") {
			t.Fatalf("replace must preserve exact digits, got %#v", p2[0].Value)
		}
	})

	t.Run("risk3a_inplace_compaction_aliases_caller_buffer", func(t *testing.T) {
		// json.go:compactInPlace (json.go:133) rewrites the
		// input slice in place. CompareJSON hands the caller
		// buffer to the differ, so the caller observes the
		// mutation: shared-state risk.
		callerSrc := []byte(`{"a": 1, "b": 2}`)
		callerTgt := []byte(`{"a":1,"b":9}`)
		snapshot := bytes.Clone(callerTgt)
		if _, err := CompareJSON(callerSrc, callerTgt, Rationalize(), InPlaceCompaction()); err != nil {
			t.Fatalf("CompareJSON error: %v", err)
		}
		if bytes.Equal(callerTgt, snapshot) {
			t.Fatal("InPlaceCompaction must be documented as mutating the caller buffer")
		}
		t.Logf("caller target buffer mutated in place: %q -> %q", snapshot, callerTgt)
	})

	t.Run("risk3b_test_op_does_not_check_value_on_apply", func(t *testing.T) {
		// apply.go only verifies that a test path exists
		// (apply.go:71-80); a wrong precondition value is
		// silently accepted, so a test op cannot be treated
		// as a checked assertion by consumers.
		p, err := CompareJSON([]byte(`{"a":1}`), []byte(`{"a":2}`), Invertible())
		if err != nil {
			t.Fatalf("CompareJSON error: %v", err)
		}
		got, err := p.apply([]byte(`{"a":99}`), false)
		if err != nil {
			t.Fatalf("unexpected error on violated test precondition: %v", err)
		}
		if string(compactJSON(t, got)) != `{"a":2}` {
			t.Fatalf("document changed despite failed precondition: %s", got)
		}
		t.Logf("documented: test op at /a passed against {\"a\":99}; precondition value is not verified")
		// The patch still round-trips when applied to the
		// document it was generated for.
		if ok, msg := roundTrip(t, p, []byte(`{"a":1}`), []byte(`{"a":2}`)); !ok {
			t.Fatalf("certified input must round-trip: %s", msg)
		}
	})
}

func mustUseNumber(t *testing.T, b []byte, v *any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}

// TestAuditRecoverableFailureNoLeakedState injects a
// recoverable failure mid-run (invalid JSON type that panics
// deep inside node comparison, per equal.go deepEqualValue),
// verifies the failure is surfaced through the public error
// layer, proves the partially-updated shared Differ state is
// observable before Reset, and shows that resetting and
// retrying with the exact same valid inputs yields the stable
// success result with no leftover cache, counter or temp state.
func TestAuditRecoverableFailureNoLeakedState(t *testing.T) {
	t.Run("mid_flight_failure_leaves_partial_patch_then_reset_recovers", func(t *testing.T) {
		d := &Differ{}

		// deepEqualValue scans arrays index 0 upward, so the
		// invalid node sits at the tail. Indices 0 and 1 are
		// valid, differing pairs: compareArrays emits two
		// replacements first, then index 2 panics deep inside
		// deepEqualValue (equal.go), leaving the shared Differ
		// half-updated.
		src := []any{1, "s", invalidJSONNode{}}
		tgt := []any{2, "t", invalidJSONNode{}}

		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected recoverable panic from invalid JSON node")
				} else {
					t.Logf("recovered mid-flight failure as: %v", r)
				}
			}()
			d.Compare(src, tgt)
			t.Fatal("Compare should not return normally")
		}()
		if len(d.patch) != 2 ||
			d.patch[0].Type != OperationReplace || d.patch[0].Path != "/0" ||
			d.patch[1].Type != OperationReplace || d.patch[1].Path != "/1" {
			t.Fatalf("half-updated patch must contain the earlier /0 and /1 replaces, got %s", d.patch)
		}
		if d.patch[0].Value != float64(2) || d.patch[1].Value != "t" {
			t.Fatalf("earlier replacements must carry their values: %#v %#v",
				d.patch[0].Value, d.patch[1].Value)
		}
		// Same input through the public wrapped entry returns
		// an error belonging to the existing error layer
		// instead of panicking across the API boundary.
		if _, err := CompareWithoutMarshal(src, tgt); err == nil ||
			!strings.Contains(err.Error(), "jsondiff: invalid json type") {
			t.Fatalf("expected invalid json type error from existing layer, got %v", err)
		}

		// Recover: reset clears patch, pointer and factorize
		// cache, then retry with a valid input pair.
		d.Reset()
		if len(d.patch) != 0 || len(d.hashmap) != 0 || d.ptr.sep != 0 || len(d.ptr.buf) != 0 {
			t.Fatalf("Reset must clear patch/cache/pointer: patch=%d hashmap=%d sep=%d buf=%d",
				len(d.patch), len(d.hashmap), d.ptr.sep, len(d.ptr.buf))
		}
		d.targetBytes = []byte(`[2,3]`)
		d.Compare([]any{1, 2, 3}, []any{2, 3})

		fresh, err := CompareJSON([]byte(`[1,2,3]`), []byte(`[2,3]`))
		if err != nil {
			t.Fatalf("reference CompareJSON error: %v", err)
		}
		if d.patch.String() != fresh.String() {
			t.Fatalf("recovered retry differs from a fresh differ:\n retry %s\n fresh %s",
				d.patch, fresh)
		}
		if d.snapshotPatchLen != 0 {
			t.Fatalf("LCS counter leaked from failed run: %d", d.snapshotPatchLen)
		}
	})

	t.Run("factorize_failure_clears_hashmap_on_reset", func(t *testing.T) {
		// prepare (differ.go:156) visits common child arrays:
		// index 0 is deeply equal and is indexed in the
		// factorize hashmap; at index 1 both documents carry
		// an invalid node, which aborts the prepare pass and
		// leaves the cache partially populated.
		d := &Differ{}
		d.WithOpts(Factorize())
		d.targetBytes = []byte(`[[1,2,3],9]`)
		// Common prefix array [1,2,3] is indexed by prepare;
		// at index 1 both documents carry an invalid node,
		// which aborts the prepare pass after the cache write.
		src := []any{[]any{1, 2, 3}, invalidJSONNode{}}
		tgt := []any{[]any{1, 2, 3}, invalidJSONNode{}}

		func() {
			defer func() { _ = recover() }()
			d.Compare(src, tgt)
		}()
		if len(d.hashmap) == 0 {
			t.Fatal("expected factorize cache populated before the failure")
		}
		d.Reset()
		if len(d.hashmap) != 0 || len(d.patch) != 0 || d.ptr.sep != 0 || len(d.ptr.buf) != 0 {
			t.Fatalf("Reset must clear the factorize cache too: hashmap=%d patch=%d",
				len(d.hashmap), len(d.patch))
		}
		// Retry with identical valid inputs and compare every
		// internal artifact against a fresh Differ.
		d.targetBytes = []byte(`[1,2,3]`)
		d.Compare([]any{1, 2, 3}, []any{1, 2, 3})

		ref := &Differ{}
		ref.WithOpts(Factorize())
		ref.targetBytes = []byte(`[1,2,3]`)
		ref.Compare([]any{1, 2, 3}, []any{1, 2, 3})
		if len(d.patch) != 0 || len(ref.patch) != 0 {
			t.Fatalf("equal inputs must yield an empty patch: retry=%s ref=%s", d.patch, ref.patch)
		}
		if len(d.hashmap) != len(ref.hashmap) {
			t.Fatalf("factorize cache after success differs from fresh: %d vs %d",
				len(d.hashmap), len(ref.hashmap))
		}
	})

	t.Run("lcs_counter_reset_between_runs", func(t *testing.T) {
		d := &Differ{}
		d.WithOpts(LCS(), Factorize())
		d.targetBytes = []byte(`[3,2,1]`)
		d.Compare([]any{1, 2, 3}, []any{3, 2, 1})
		if d.snapshotPatchLen != 0 {
			t.Fatalf("snapshotPatchLen must be restored after a successful LCS run, got %d",
				d.snapshotPatchLen)
		}
		first := d.patch.String()

		// A second run on the same reused Differ without
		// Reset must not be affected by the previous run
		// (Compare starts appending at the retained slice;
		// the audit pins the documented reuse contract:
		// reuse is correct after Reset).
		d.Reset()
		d.targetBytes = []byte(`[3,2,1]`)
		d.Compare([]any{1, 2, 3}, []any{3, 2, 1})
		if d.patch.String() != first {
			t.Fatalf("reused+reset differ is unstable:\n first %s\n retry %s", first, d.patch)
		}
		if d.snapshotPatchLen != 0 {
			t.Fatalf("LCS insertion counter leaked: %d", d.snapshotPatchLen)
		}
	})
}

// invalidJSONNode is a value that encoding/json can marshal
// through Compare, but which the node comparison layer
// (jsonTypeSwitch, equal.go) cannot classify. It models an
// internal, recoverable failure for the audit without forging
// any terminal patch object.
type invalidJSONNode struct{}

func (invalidJSONNode) MarshalJSON() ([]byte, error) {
	return []byte(`42`), nil
}

// TestAuditOrderStableAcrossScheduling is the second fixed
// group: it only changes enumeration/scheduling conditions
// that are unrelated to the result (Go map enumeration order
// across goroutines, the order of option arguments, and the
// choice between equivalent public entries) and asserts that
// the ordered operation set stays identical.
func TestAuditOrderStableAcrossScheduling(t *testing.T) {
	// Many keys force compareObjects (differ.go:231) through
	// its sorted-key enumeration regardless of Go's random
	// map iteration.
	const keyCount = 40
	build := func(mutate bool) map[string]any {
		m := make(map[string]any, keyCount)
		for i := 0; i < keyCount; i++ {
			k := fmt.Sprintf("k%02d", i)
			v := any(i)
			if mutate && i%3 == 0 {
				v = i * 10
			}
			if mutate && i%7 == 0 {
				delete(m, k)
				continue
			}
			m[k] = v
		}
		if mutate {
			m["zz_new"] = "added"
		}
		return m
	}
	srcObj, tgtObj := build(false), build(true)
	srcB, _ := json.Marshal(srcObj)
	tgtB, _ := json.Marshal(tgtObj)

	ref, err := CompareJSON(srcB, tgtB)
	if err != nil {
		t.Fatalf("reference CompareJSON error: %v", err)
	}
	if ok, msg := roundTrip(t, ref, srcB, tgtB); !ok {
		t.Fatalf("reference patch must round-trip: %s", msg)
	}
	refSig := signature(ref)
	// Internal invariant: operations are sorted by the key
	// enumeration, i.e. lexicographically by path.
	for i := 1; i < len(refSig); i++ {
		if refSig[i-1].path > refSig[i].path {
			t.Fatalf("operations are not path-sorted at #%d: %q > %q",
				i, refSig[i-1].path, refSig[i].path)
		}
	}

	t.Run("concurrent_barrier_schedules_all_goroutines_together", func(t *testing.T) {
		const workers = 16
		var start sync.WaitGroup
		var ready sync.WaitGroup
		var done sync.WaitGroup
		start.Add(1)
		ready.Add(workers)
		done.Add(workers)
		sigs := make([][]opSig, workers)
		for w := 0; w < workers; w++ {
			go func(id int) {
				defer done.Done()
				ready.Done()
				start.Wait() // barrier: all workers race together
				p, err := CompareJSON(srcB, tgtB, Rationalize())
				if err != nil {
					t.Errorf("worker %d CompareJSON error: %v", id, err)
					return
				}
				sigs[id] = signature(p)
			}(w)
		}
		ready.Wait()
		start.Done()
		done.Wait()
		for i := 1; i < workers; i++ {
			if j := firstSigMismatch(sigs[0], sigs[i]); j != -1 {
				t.Fatalf("worker %d diverges at op #%d:\n %v\n %v",
					i, j, sigOrNil(sigs[0], j), sigOrNil(sigs[i], j))
			}
		}
	})

	t.Run("option_argument_order_is_result_irrelevant", func(t *testing.T) {
		p1, err := CompareJSON(srcB, tgtB, Factorize(), Invertible(), LCS())
		if err != nil {
			t.Fatalf("p1 error: %v", err)
		}
		p2, err := CompareJSON(srcB, tgtB, LCS(), Invertible(), Factorize())
		if err != nil {
			t.Fatalf("p2 error: %v", err)
		}
		p3, err := CompareJSON(srcB, tgtB, Invertible(), Factorize(), LCS())
		if err != nil {
			t.Fatalf("p3 error: %v", err)
		}
		if i := firstSigMismatch(signature(p1), signature(p2)); i != -1 {
			t.Fatalf("option permutation changed result at #%d", i)
		}
		if i := firstSigMismatch(signature(p1), signature(p3)); i != -1 {
			t.Fatalf("option permutation changed result at #%d", i)
		}
	})

	t.Run("equivalent_public_entries_same_ordered_patch", func(t *testing.T) {
		// Compare and CompareJSON normalize differently in
		// mechanism but identically in result: changing the
		// entry/scheduling condition must not reorder ops.
		pFromValues, err := Compare(srcObj, tgtObj)
		if err != nil {
			t.Fatalf("Compare error: %v", err)
		}
		pFromJSON, err := CompareJSON(srcB, tgtB)
		if err != nil {
			t.Fatalf("CompareJSON error: %v", err)
		}
		pReuse := func() Patch {
			var d Differ
			p, err := compare(&d, srcObj, tgtObj)
			if err != nil {
				t.Fatalf("compare error: %v", err)
			}
			return p
		}()
		if i := firstSigMismatch(signature(pFromValues), signature(pFromJSON)); i != -1 {
			t.Fatalf("Compare vs CompareJSON diverge at #%d", i)
		}
		if i := firstSigMismatch(signature(pFromValues), signature(pReuse)); i != -1 {
			t.Fatalf("Compare vs internal entry diverge at #%d", i)
		}
	})
}

// TestAuditSeededRoundTrip generates random JSON-like inputs
// from a fixed seed and certifies apply(diff(a,b),a)=b for
// every option set that does not opt into a documented risk.
// On failure the seed, iteration, option set and minimal
// input pair are printed so the case is reproducible.
func TestAuditSeededRoundTrip(t *testing.T) {
	const fixedSeed = uint64(0x5eeda0d1)
	const iterations = 200

	optionSets := [][]Option{
		nil,
		{LCS()},
		{Factorize()},
		{LCS(), Factorize()},
		{Invertible()},
		{Factorize(), Invertible()},
		{LCS(), Factorize(), Invertible()},
		{Rationalize(), SkipCompact()},
	}

	for seed := uint64(0); seed < iterations; seed++ {
		rng := rand.New(rand.NewPCG(fixedSeed, seed))
		depth := rng.IntN(3)
		src := genAuditValue(rng, depth)
		tgt := mutateAuditValue(rng, cloneAuditValue(src), rng.IntN(4)+1)
		srcB, err := json.Marshal(src)
		if err != nil {
			t.Fatalf("seed %d marshal src: %v", seed, err)
		}
		tgtB, err := json.Marshal(tgt)
		if err != nil {
			t.Fatalf("seed %d marshal tgt: %v", seed, err)
		}
		for oi, opts := range optionSets {
			p, err := CompareJSON(srcB, tgtB, opts...)
			if err != nil {
				t.Fatalf("seed %d opts %d error: %v", seed, oi, err)
			}
			if ok, msg := roundTrip(t, p, srcB, tgtB); !ok {
				t.Fatalf("seed %d (PCG seed %d) opts %d failed:\n src: %s\n tgt: %s\n %s",
					seed, fixedSeed, oi, srcB, tgtB, msg)
			}
		}
	}
}

func genAuditValue(rng *rand.Rand, depth int) any {
	if depth <= 0 || rng.IntN(3) != 0 {
		switch rng.IntN(5) {
		case 0:
			return nil
		case 1:
			return rng.IntN(3) == 0 // bias toward duplicated values
		case 2:
			return []string{"a", "b", "a", "c"}[rng.IntN(4)]
		case 3:
			// Integers stay small to avoid float64 precision
			// collisions (risk2 is covered separately).
			return float64(rng.IntN(16))
		default:
			if rng.IntN(2) == 0 {
				return nil
			}
			return []string{"a", "a", "b"}[rng.IntN(3)]
		}
	}
	switch rng.IntN(2) {
	case 0:
		n := rng.IntN(5)
		a := make([]any, n)
		for i := range a {
			a[i] = genAuditValue(rng, depth-1)
		}
		return a
	default:
		m := map[string]any{}
		n := rng.IntN(5)
		for i := 0; i < n; i++ {
			m[fmt.Sprintf("k%d", rng.IntN(6))] = genAuditValue(rng, depth-1)
		}
		return m
	}
}

func cloneAuditValue(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var c any
	if err := json.Unmarshal(b, &c); err != nil {
		panic(err)
	}
	return c
}

func mutateAuditValue(rng *rand.Rand, v any, budget int) any {
	if budget <= 0 {
		return v
	}
	switch x := v.(type) {
	case []any:
		switch rng.IntN(3) {
		case 0:
			if len(x) > 0 {
				i := rng.IntN(len(x))
				x[i] = genAuditValue(rng, 1)
			}
		case 1:
			x = append(x, genAuditValue(rng, 1))
		default:
			if len(x) > 0 {
				i := rng.IntN(len(x))
				x = append(x[:i], x[i+1:]...)
			}
		}
		for i := range x {
			if rng.IntN(3) == 0 {
				x[i] = mutateAuditValue(rng, x[i], budget-1)
			}
		}
		return x
	case map[string]any:
		switch rng.IntN(3) {
		case 0:
			x[fmt.Sprintf("n%d", rng.IntN(100))] = genAuditValue(rng, 1)
		case 1:
			for k := range x {
				delete(x, k)
				break
			}
		default:
			for k := range x {
				x[k] = genAuditValue(rng, 1)
				break
			}
		}
		return x
	default:
		return genAuditValue(rng, 1)
	}
}

// TestAuditErrorLayering verifies that explicit rejections use
// the existing, repeatable error layers rather than guessing
// success.
func TestAuditErrorLayering(t *testing.T) {
	t.Run("malformed_json_is_syntax_error", func(t *testing.T) {
		_, err := CompareJSON([]byte(`{oops`), []byte(`{}`))
		var se *json.SyntaxError
		if !errors.As(err, &se) {
			t.Fatalf("expected *json.SyntaxError from existing layer, got %T: %v", err, err)
		}
		_, err2 := CompareJSON([]byte(`{oops`), []byte(`{}`))
		if err2.Error() != err.Error() {
			t.Fatalf("rejection must be repeatable:\n %v\n %v", err, err2)
		}
	})

	t.Run("custom_unmarshal_rejection_is_surficed", func(t *testing.T) {
		// An unrecognized/unsupported input must surface as an
		// explicit rejection, never as a guessed empty patch.
		reject := UnmarshalFunc(func(b []byte, v any) error {
			return fmt.Errorf("audit: %w", errUnsupportedAudit)
		})
		if _, err := CompareJSON([]byte(`{"a":1}`), []byte(`{"a":2}`), reject); !errors.Is(err, errUnsupportedAudit) {
			t.Fatalf("expected unsupported sentinel to surface, got %v", err)
		}
		if _, err := Compare(1, 2, reject); !errors.Is(err, errUnsupportedAudit) {
			t.Fatalf("Compare must surface the same rejection, got %v", err)
		}
	})

	t.Run("invert_rejects_non_reversible_and_ambiguous_copy", func(t *testing.T) {
		rem, err := CompareJSON([]byte(`{"a":1}`), []byte(`{}`))
		if err != nil {
			t.Fatalf("CompareJSON error: %v", err)
		}
		if _, err := rem.Invert(); !errors.Is(err, ErrNonReversible) {
			t.Fatalf("remove without test must map to ErrNonReversible, got %v", err)
		}
		cp, err := CompareJSON(
			[]byte(`{"a":{"k":"v"},"c":1}`),
			[]byte(`{"a":{"k":"v"},"b":{"k":"v"},"c":1}`),
			Factorize())
		if err != nil {
			t.Fatalf("CompareJSON error: %v", err)
		}
		if _, err := cp.Invert(); !errors.Is(err, ErrAmbiguousCopyOp) {
			t.Fatalf("copy inversion must map to ErrAmbiguousCopyOp, got %v", err)
		}
		// Invertible patches invert and the inverse applied to
		// the target reproduces the source: full back-and-forth
		// round-trip through public entries + production apply.
		inv, err := CompareJSON([]byte(`{"a":1}`), []byte(`{"a":2}`), Invertible())
		if err != nil {
			t.Fatalf("CompareJSON error: %v", err)
		}
		back, err := inv.Invert()
		if err != nil {
			t.Fatalf("Invert error: %v", err)
		}
		if ok, msg := roundTrip(t, back, []byte(`{"a":2}`), []byte(`{"a":1}`)); !ok {
			t.Fatalf("inverted patch round-trip failed: %s", msg)
		}
	})
}

var errUnsupportedAudit = errors.New("unsupported input")
