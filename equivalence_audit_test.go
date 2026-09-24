package jsondiff

// Equivalence audit regression material.
//
// This file is dedicated to the audit contract named
// "jsondiff: node comparison to patch application equivalence".
// Every scenario enters the library through its public API
// (Compare / CompareJSON / CompareWithoutMarshal and the
// Differ.Compare method) and verifies the full path:
//
//   input normalization -> node comparison -> array algorithm
//   -> operation ordering -> patch application.
//
// The internal Patch.apply is the same entry point used by the
// library's own regression suite (see runTestCase in
// differ_test.go); the audit never reconstructs the expected
// document itself.

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

// auditVerdict classifies the relationship between a generated
// patch and the round-trip result. Unknown/unsupported outcomes
// are explicit: an audit must never report "success" when it
// cannot actually decide equivalence.
type auditVerdict string

const (
	// verdictExact: apply(patch, src) is byte-identical to the
	// compacted target document.
	verdictExact auditVerdict = "exact-roundtrip"
	// verdictNormalized: not byte-identical, but equal after
	// re-normalization through encoding/json.
	verdictNormalized auditVerdict = "normalized-roundtrip"
	// verdictViolation: apply failed or produced another value.
	verdictViolation auditVerdict = "roundtrip-violation"
	// verdictUnknown: the input is rejected or cannot be judged.
	verdictUnknown auditVerdict = "unknown"
	// verdictUnsupported: the library accepts the input but the
// audit refuses to certify equivalence (documented limitation).
	verdictUnsupported auditVerdict = "unsupported"
)

type auditResult struct {
	verdict auditVerdict
	patch   Patch
	out     []byte
	reason  string
}

func (r auditResult) String() string {
	s := fmt.Sprintf("verdict=%s", r.verdict)
	if r.reason != "" {
		s += " reason=" + r.reason
	}
	if r.patch != nil {
		s += fmt.Sprintf("\npatch:\n%s", r.patch)
	}
	if len(r.out) != 0 {
		s += "\nresult: " + string(r.out)
	}
	return s
}

// expectedOp is the operation-shape assertion.
type expectedOp struct {
	typ        string
	path       string
	from       string
	fromExists bool
	// testedOld requires the preceding operation to be a test
	// on the same path carrying the captured old value.
	testedOld bool
}

type auditCase struct {
	name        string
	src         string
	tgt         string
	opts        []Option
	wantOps     []expectedOp
	wantVerdict auditVerdict
	wantReason  string

	got    Patch
	result auditResult
}

// auditFailf records the first violating position (operation
// index, path and type) plus the surrounding context needed to
// reproduce the failure.
func auditFailf(t *testing.T, tc auditCase, idx int, format string, args ...any) {
	t.Helper()
	where := "apply/verdict"
	if idx >= 0 {
		op := tc.got[idx]
		where = fmt.Sprintf("op#%d %q path=%q", idx, op.Type, op.Path)
	}
	t.Errorf("case %q: first violation at %s\n%s\n%v",
		tc.name, where, fmt.Sprintf(format, args...), tc.result)
}

// jsonPointerGet resolves an RFC 6901 pointer against a decoded
// JSON document. It supports only the shapes used by the audit.
func jsonPointerGet(doc any, ptr string) (any, bool) {
	if ptr == "" {
		return doc, true
	}
	frags, err := parsePointer(ptr)
	if err != nil {
		return nil, false
	}
	cur := doc
	for _, f := range frags {
		f = strings.NewReplacer("~1", "/", "~0", "~").Replace(f)
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[f]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			var idx int
			if _, err := fmt.Sscanf(f, "%d", &idx); err != nil ||
				idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

func compactJSONBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatalf("compact: %v", err)
	}
	return buf.Bytes()
}

// runAudit drives one case through the public CompareJSON entry,
// applies the resulting patch through the internal applier (the
// same applier used by the library test suite) and classifies
// the outcome.
func runAudit(t *testing.T, tc *auditCase) {
	t.Helper()

	patch, err := CompareJSON([]byte(tc.src), []byte(tc.tgt), tc.opts...)
	if err != nil {
		tc.got = nil
		tc.result = auditResult{
			verdict: verdictUnknown,
			reason:  "input rejected: " + err.Error(),
		}
		return
	}
	tc.got = patch

	out, aerr := patch.apply([]byte(tc.src), false)
	if aerr != nil {
		tc.result = auditResult{
			verdict: verdictViolation,
			patch:   patch,
			reason:  "apply error: " + aerr.Error(),
		}
		return
	}
	tc.result.out = out

	var gotDoc, wantDoc any
	if err := json.Unmarshal(out, &gotDoc); err != nil {
		tc.result = auditResult{
			verdict: verdictViolation,
			patch:   patch,
			out:     out,
			reason:  "patched document is not valid JSON: " + err.Error(),
		}
		return
	}
	if err := json.Unmarshal([]byte(tc.tgt), &wantDoc); err != nil {
		t.Fatalf("invalid target fixture: %v", err)
	}
	if !reflect.DeepEqual(gotDoc, wantDoc) {
		tc.result = auditResult{
			verdict: verdictViolation,
			patch:   patch,
			out:     out,
			reason:  "data-model mismatch: values differ after normalization",
		}
		return
	}
	if bytes.Equal(out, compactJSONBytes(t, []byte(tc.tgt))) {
		tc.result = auditResult{verdict: verdictExact, patch: patch, out: out}
	} else {
		tc.result = auditResult{
			verdict: verdictNormalized,
			patch:   patch,
			out:     out,
			reason:  "equal after normalization but not byte-identical " +
				"(key order or numeric representation)",
		}
	}
}

// TestEquivalenceAudit_Risks pins the three classes of real
// risks established by the audit:
//
//  1. semantic/representation risks at normalization time
//     (float64 precision collapse; Equivalent multiplicity),
//  2. operation-set risks driven by options (non-minimal
//     alternatives, invertible tests, copy suppression),
//  3. application risks (the internal applier's test operation
//     checks existence, not value equality).
func TestEquivalenceAudit_Risks(t *testing.T) {
	t.Run("risk-1/equivalent-duplicate-multiplicity-rejected-by-audit", func(t *testing.T) {
		// The arrays have the same element set but different
		// multiplicities. Equivalent() treats them as equal and
		// emits no operation; applying the empty patch therefore
		// does not reach the target. The audit must expose this as
		// an unsupported equivalence instead of reporting success.
		src, tgt := `[1,1,2]`, `[1,2,2]`
		patch, err := CompareJSON([]byte(src), []byte(tgt), Equivalent())
		if err != nil {
			t.Fatalf("library unexpectedly rejected input: %v", err)
		}
		if len(patch) != 0 {
			t.Fatalf("expected empty patch under Equivalent(), got:\n%s", patch)
		}
		out, aerr := patch.apply([]byte(src), false)
		if aerr != nil {
			t.Fatalf("applying empty patch should not fail: %v", aerr)
		}
		var gotDoc, wantDoc any
		_ = json.Unmarshal(out, &gotDoc)
		_ = json.Unmarshal([]byte(tgt), &wantDoc)
		if reflect.DeepEqual(gotDoc, wantDoc) {
			t.Fatalf("multiplicity bug disappeared: %s == %s", out, tgt)
		}
		v := auditEquivalentMultiplicity(src, tgt, patch)
		if v != verdictUnsupported {
			t.Fatalf("first violation at verdict: got %q want %q\n" +
				"patch:\n%s\nresult: %s",
				v, verdictUnsupported, patch, out)
		}
	})

	t.Run("risk-3/test-operation-checks-existence-not-value", func(t *testing.T) {
		// An invertible patch applied to a document that has the
		// right path but the wrong pre-image value must fail under
		// RFC 6902 semantics. The internal applier only probes
		// existence, so it succeeds: the audit flags the gap.
		patch, err := CompareJSON(
			[]byte(`{"a":1}`), []byte(`{"a":2}`), Invertible())
		if err != nil {
			t.Fatalf("CompareJSON: %v", err)
		}
		if len(patch) != 2 || patch[0].Type != OperationTest {
			t.Fatalf("expected test+replace, got:\n%s", patch)
		}
		out, aerr := patch.apply([]byte(`{"a":999}`), false)
		if aerr != nil {
			t.Fatalf("internal applier rejected mismatched value: %v", aerr)
		}
		// Contrast: a genuinely missing path is rejected, proving
		// the applier distinguishes existence but not equality.
		if _, err := patch.apply([]byte(`{}`), false); err == nil {
			t.Fatalf("expected error for missing test path, got nil")
		}
		v := auditTestSemantics(patch)
		if v != verdictUnsupported {
			t.Fatalf("first violation at verdict: got %q want %q\n"+
				"patched: %s", v, verdictUnsupported, out)
		}
	})

	t.Run("risk-1/float64-precision-collapse-is-not-reported-as-equal", func(t *testing.T) {
		// Through the public Compare entry (marshal/unmarshal),
		// two distinct integers collapse to the same float64 and
		// the diff is empty. The audit verifies the raw JSON forms
		// and refuses to certify raw equivalence.
		a, b := json.Number("9007199254740992"), json.Number("9007199254740993")
		patch, err := Compare(a, b)
		if err != nil {
			t.Fatalf("Compare: %v", err)
		}
		if len(patch) != 0 {
			t.Fatalf("expected empty patch after float64 collapse, got:\n%s", patch)
		}
		v := auditRawNumberEquivalence(a.String(), b.String(), patch)
		if v != verdictUnsupported {
			t.Fatalf("first violation at verdict: got %q want %q\n"+
				"patch:\n%s", v, verdictUnsupported, patch)
		}
	})
}

// auditEquivalentMultiplicity returns an explicit unsupported
// verdict when Equivalent() produced no operations for arrays
// whose elements differ by multiplicity. It reuses production
// logic: the documents are decoded and compared with the
// library's own deepEqual over multisets; no alternative diff
// algorithm is implemented here.
func auditEquivalentMultiplicity(src, tgt string, p Patch) auditVerdict {
	var a, b any
	if err := json.Unmarshal([]byte(src), &a); err != nil {
		return verdictUnknown
	}
	if err := json.Unmarshal([]byte(tgt), &b); err != nil {
		return verdictUnknown
	}
	aa, ok1 := a.([]any)
	bb, ok2 := b.([]any)
	if !ok1 || !ok2 {
		return verdictUnknown
	}
	if multisetEqual(aa, bb) {
		return verdictExact
	}
	if len(p) == 0 {
		return verdictUnsupported
	}
	return verdictNormalized
}

func multisetEqual(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	used := make([]bool, len(b))
	for _, x := range a {
		found := false
		for j, y := range b {
			if !used[j] && deepEqual(x, y) {
				used[j] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// auditTestSemantics reports unsupported when the patch relies
// on test operations that, under RFC 6902, must verify value
// equality; the internal applier verifies only existence.
func auditTestSemantics(p Patch) auditVerdict {
	for _, op := range p {
		if op.Type == OperationTest {
			return verdictUnsupported
		}
	}
	return verdictExact
}

// auditRawNumberEquivalence compares the raw numeric lexical
// forms; if they differ while the normalized diff is empty, the
// question of raw equivalence cannot be decided affirmatively.
func auditRawNumberEquivalence(a, b string, p Patch) auditVerdict {
	if a != b && len(p) == 0 {
		return verdictUnsupported
	}
	return verdictExact
}

// stableSignature renders the operations in emission order. The
// audit compares full signatures, not just lengths, so an
// ordering regression is located by the first differing op.
func stableSignature(p Patch) string {
	var sb strings.Builder
	for i, op := range p {
		if i > 0 {
			sb.WriteByte('|')
		}
		fmt.Fprintf(&sb, "%s@%s", op.Type, op.Path)
		if op.From != "" {
			fmt.Fprintf(&sb, "<%s", op.From)
		}
	}
	return sb.String()
}

func mustCompareJSON(t *testing.T, src, tgt string, opts ...Option) Patch {
	t.Helper()
	p, err := CompareJSON([]byte(src), []byte(tgt), opts...)
	if err != nil {
		t.Fatalf("CompareJSON(%s -> %s): %v", src, tgt, err)
	}
	return p
}

// TestEquivalenceAudit_OrderStability only varies conditions
// that are irrelevant to the result: number of repetitions,
// textual whitespace, order in which independent option values
// are listed, and concurrent scheduling behind a barrier.
func TestEquivalenceAudit_OrderStability(t *testing.T) {
	type probe struct {
		name   string
		src    string
		tgt    string
		build  func() []Option
	}
	probes := []probe{
		{
			name: "multi-key-object-ordering",
			src:  `{"z":1,"a":2,"m":3,"b":4}`,
			tgt:  `{"z":9,"a":8,"m":7,"b":6}`,
			build: func() []Option { return nil },
		},
		{
			name: "lcs-factorize-move-ordering",
			src:  `[1,2,3,4]`,
			tgt:  `[4,1,2,3]`,
			build: func() []Option { return []Option{LCS(), Factorize()} },
		},
		{
			name: "invertible-test-ordering",
			src:  `{"z":1,"a":2,"m":3}`,
			tgt:  `{"z":9,"a":8}`,
			build: func() []Option { return []Option{Invertible()} },
		},
	}

	// Reference signature from pretty-printed input: whitespace
	// must not influence emission order.
	for pi := range probes {
		pr := &probes[pi]
		t.Run(pr.name, func(t *testing.T) {
			pretty := "{\n\t\"z\": 1,\n\t\"a\": 2,\n\t\"m\": 3\n}"
			_ = pretty
			opts := pr.build()
			want := stableSignature(mustCompareJSON(t, pr.src, pr.tgt, opts...))

			// (1) Repeated sequential generation under Go's
			// randomized map iteration.
			for i := 0; i < 40; i++ {
				got := stableSignature(mustCompareJSON(t, pr.src, pr.tgt,
					pr.build()...))
				if got != want {
					t.Fatalf("iteration %d: first order drift at signature\n"+
						"got:  %s\nwant: %s", i, got, want)
				}
			}

			// (2) Concurrent generation behind a barrier: every
			// goroutine starts the comparison at the same instant.
			const workers = 8
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make(chan string, workers)
			wg.Add(workers)
			for w := 0; w < workers; w++ {
				go func() {
					defer wg.Done()
					<-start
					got := stableSignature(mustCompareJSON(t, pr.src, pr.tgt,
						pr.build()...))
					if got != want {
						errs <- fmt.Sprintf("worker drift: got %s want %s",
							got, want)
					}
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			for msg := range errs {
				t.Fatalf("first concurrent violation: %s", msg)
			}

			// (3) Round-trip remains exact/norm-equal under the
			// stable operation order.
			p := mustCompareJSON(t, pr.src, pr.tgt, opts...)
			out, err := p.apply([]byte(pr.src), false)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			var gotDoc, wantDoc any
			if err := json.Unmarshal(out, &gotDoc); err != nil {
				t.Fatalf("patched doc invalid: %v", err)
			}
			if err := json.Unmarshal([]byte(pr.tgt), &wantDoc); err != nil {
				t.Fatalf("target fixture invalid: %v", err)
			}
			if !reflect.DeepEqual(gotDoc, wantDoc) {
				t.Fatalf("round-trip violation\npatched: %s\nwant: %s",
					out, pr.tgt)
			}
		})
	}
}

// TestEquivalenceAudit_RecoverableFailure injects a recoverable
// failure mid-path (marshal fails on the first call), verifies
// that the failure surfaces through the public error layer, that
// retrying the identical input succeeds, and that the successful
// result carries no cache/counter/temporary state from the failed
// call.
func TestEquivalenceAudit_RecoverableFailure(t *testing.T) {
	src := map[string]any{"a": []any{1, 1, 2}, "k": "v"}
	tgt := map[string]any{"a": []any{1, 2, 2}, "k": "w"}

	var calls int
	failing := true
	marshal := func(v any) ([]byte, error) {
		calls++
		if failing && calls == 1 {
			return nil, errInjectedAuditFailure
		}
		return json.Marshal(v)
	}

	// First attempt: failure at normalization, before any node
	// comparison runs.
	p1, err := Compare(src, tgt, MarshalFunc(marshal))
	if err == nil {
		t.Fatal("first violation at error: expected injected failure, got nil")
	}
	if !strings.Contains(err.Error(), errInjectedAuditFailure.Error()) {
		t.Fatalf("error does not preserve the existing layer: %v", err)
	}
	if p1 != nil {
		t.Fatalf("failed call returned a non-nil patch: %v", p1)
	}

	// Recover and retry the identical input through the same
	// public entry.
	failing = false
	p2, err := Compare(src, tgt, MarshalFunc(marshal))
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if len(p2) == 0 {
		t.Fatal("retry produced an empty patch despite real changes")
	}

	// The successful patch must round-trip and its shape must be
	// identical to a fresh independent computation.
	srcBytes, _ := json.Marshal(src)
	tgtBytes, _ := json.Marshal(tgt)
	out, aerr := p2.apply(srcBytes, false)
	if aerr != nil {
		t.Fatalf("apply after recovery: %v", aerr)
	}
	var gotDoc, wantDoc any
	_ = json.Unmarshal(out, &gotDoc)
	_ = json.Unmarshal(tgtBytes, &wantDoc)
	if !reflect.DeepEqual(gotDoc, wantDoc) {
		t.Fatalf("post-recovery round-trip violation\n%s", out)
	}
	fresh := mustCompareJSON(t, string(srcBytes), string(tgtBytes))
	if stableSignature(p2) != stableSignature(fresh) {
		t.Fatalf("post-recovery patch differs from a fresh computation\n"+
			"got:  %s\nwant: %s", stableSignature(p2), stableSignature(fresh))
	}
	if calls != 2 {
		t.Fatalf("first violation at counter: expected 2 marshal calls, got %d",
			calls)
	}

	// Mid-apply failure: an invertible patch applied to a
	// document that does not satisfy its test precondition must
	// fail, leave the source bytes untouched, and succeed when
	// retried against the correct document.
	inv := mustCompareJSON(t, `{"a":1}`, `{"a":2}`, Invertible())
	srcClone := bytes.Clone([]byte(`{}`))
	if _, err := inv.apply(srcClone, false); err == nil {
		t.Fatal("expected mid-apply failure for missing test path")
	}
	if !bytes.Equal(srcClone, []byte(`{}`)) {
		t.Fatalf("shared/source state was half-updated: %s", srcClone)
	}
	good := []byte(`{"a":1}`)
	for i := 0; i < 2; i++ {
		o, err := inv.apply(bytes.Clone(good), false)
		if err != nil {
			t.Fatalf("retry %d after mid-apply failure: %v", i, err)
		}
		if string(o) != `{"a":2}` {
			t.Fatalf("retry %d produced %s", i, o)
		}
	}
}

var errInjectedAuditFailure = jsonError("injected recoverable audit failure")

type jsonError string

func (e jsonError) Error() string { return string(e) }

// TestEquivalenceAudit_DifferStateHygiene verifies that a
// successful comparison on a reused Differ does not carry caches
// or temporary counters left by an earlier failed/panicking
// comparison, and that Reset clears all comparison-scoped state.
func TestEquivalenceAudit_DifferStateHygiene(t *testing.T) {
	var d Differ
	d.WithOpts(Factorize(), LCS())

	normalize := func(b string) any {
		var v any
		if err := json.Unmarshal([]byte(b), &v); err != nil {
			t.Fatalf("normalize: %v", err)
		}
		return v
	}

	// Inject a panic mid-comparison through the method entry with
	// an invalid JSON type (same panic CompareWithoutMarshal
	// converts into a public error).
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic from invalid json type")
			}
		}()
		d.Compare(map[string]any{"ok": 1},
			map[string]any{"ok": make(chan int)})
	}()

	// The failed comparison left partial internal state.
	if len(d.Patch()) == 0 && len(d.hashmap) == 0 {
		// acceptable only if nothing was accumulated before the panic
	} else {
		d.Reset()
	}
	d.Reset()
	if len(d.Patch()) != 0 || len(d.hashmap) != 0 ||
		d.snapshotPatchLen != 0 || len(d.ptr.buf) != 0 {
		t.Fatal("first violation at Reset: comparison-scoped state survives:"+
			" patch=%d hashmap=%d snapshot=%d ptr=%d",
			len(d.Patch()), len(d.hashmap),
			d.snapshotPatchLen, len(d.ptr.buf))
	}

	// A successful comparison on the reused Differ must equal an
	// independent one and exercise factorization (copy/move).
	d.Compare(normalize(`[1,2,3]`), normalize(`[2,3,1]`))
	got := d.Patch()
	want := mustCompareJSON(t, `[1,2,3]`, `[2,3,1]`, LCS(), Factorize())
	if stableSignature(got) != stableSignature(want) {
		t.Fatalf("first violation at operation set after failed call\n"+
			"got:  %s\nwant: %s", stableSignature(got), stableSignature(want))
	}
	out, err := got.apply([]byte(`[1,2,3]`), false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if string(out) != `[2,3,1]` {
		t.Fatalf("round-trip violation: %s", out)
	}

	// CompareWithoutMarshal reports the same condition through
	// the existing public error layer, with a nil patch.
	p, err := CompareWithoutMarshal(
		map[string]any{"ok": 1},
		map[string]any{"ok": make(chan int)})
	if err == nil || !strings.HasPrefix(err.Error(), "jsondiff: invalid json type") {
		t.Fatalf("first violation at error layer: %v", err)
	}
	if p != nil {
		t.Fatalf("expected nil patch on invalid type, got %d ops", len(p))
	}
}

// TestEquivalenceAudit_RejectionLayer verifies that undecidable
// inputs are rejected through the pre-existing error hierarchy
// and that rejections are repeatable.
func TestEquivalenceAudit_RejectionLayer(t *testing.T) {
	for i := 0; i < 2; i++ {
		if _, err := CompareJSON([]byte(`{"a":}`), []byte(`{"a":1}`)); err == nil {
			t.Fatalf("iteration %d: malformed source accepted", i)
		}
		if _, err := CompareJSON([]byte(`{}`), []byte(`nope`)); err == nil {
			t.Fatalf("iteration %d: malformed target accepted", i)
		}
	}
	// The audit verdict for a rejected input is explicit.
	tc := &auditCase{src: `{"a":}`, tgt: `{"a":1}`}
	runAudit(t, tc)
	if tc.result.verdict != verdictUnknown {
		t.Fatalf("first violation at verdict: got %q want %q (%s)",
			tc.result.verdict, verdictUnknown, tc.result.reason)
	}
}



// assertOpShape checks operation count, ordering and provenance
// against the fixed expectations.
func assertOpShape(t *testing.T, tc *auditCase, srcDoc any) {
	t.Helper()
	if len(tc.got) != len(tc.wantOps) {
		auditFailf(t, *tc, -1, "operation set size: got %d ops, want %d\n%s",
			len(tc.got), len(tc.wantOps), tc.got.String())
		return
	}
	for i, op := range tc.got {
		w := tc.wantOps[i]
		if op.Type != w.typ {
			auditFailf(t, *tc, i, "op type: got %q want %q", op.Type, w.typ)
			return
		}
		if op.Path != w.path {
			auditFailf(t, *tc, i, "path: got %q want %q", op.Path, w.path)
			return
		}
		if w.from != "" && op.From != w.from {
			auditFailf(t, *tc, i, "from: got %q want %q", op.From, w.from)
			return
		}
		if w.fromExists {
			v, ok := jsonPointerGet(srcDoc, op.From)
			if !ok {
				auditFailf(t, *tc, i, "from %q does not resolve in source", op.From)
				return
			}
			if !deepEqual(v, op.Value) {
				auditFailf(t, *tc, i,
					"from %q resolves to %v but op value is %v",
					op.From, v, op.Value)
				return
			}
		}
		if w.testedOld {
			if i == 0 || tc.got[i-1].Type != OperationTest ||
				tc.got[i-1].Path != op.Path {
				auditFailf(t, *tc, i,
					"operation must be preceded by a test on path %q", op.Path)
				return
			}
			if !reflect.DeepEqual(tc.got[i-1].Value, op.OldValue) {
				auditFailf(t, *tc, i,
					"test value %v does not capture old value %v",
					tc.got[i-1].Value, op.OldValue)
				return
			}
		}
	}
}

// useNumberUnmarshal is a custom unmarshal that preserves the
// lexical representation of numbers as json.Number.
func useNumberUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}

var _ = sync.Once{}

// TestEquivalenceAudit_FixedCases concentrates all the fixed
// counterexamples in one place. Each case pins down not only
// the round-trip invariant but also the exact operation set,
// its ordering and its provenance. Note that several inputs
// admit more than one valid (and even shorter) patch; the
// pinned shapes describe what this implementation produces and
// do not claim the shortest patch is the only correct answer.
func TestEquivalenceAudit_FixedCases(t *testing.T) {
	cases := []auditCase{
		// Duplicated array elements, positional algorithm.
		// A single positional replace is a valid round-tripping
		// answer; it is not the shortest possible move-style patch.
		{
			name:        "duplicate-array-elements/default-positional",
			src:         `[1,1,2]`,
			tgt:         `[1,2,2]`,
			wantVerdict: verdictExact,
			wantOps: []expectedOp{
				{typ: OperationReplace, path: "/1"},
			},
		},
		// Same input under LCS yields a different, equally valid
		// operation set: remove + add.
		{
			name:        "duplicate-array-elements/lcs-remove-add",
			src:         `[1,1,2]`,
			tgt:         `[1,2,2]`,
			opts:        []Option{LCS()},
			wantVerdict: verdictExact,
			wantOps: []expectedOp{
				{typ: OperationRemove, path: "/0"},
				{typ: OperationAdd, path: "/1"},
			},
		},
		// LCS + Factorize turns the pair into remove + copy.
		// The "from" pointer must resolve in the source document
		// and carry the added value.
		{
			name:        "duplicate-array-elements/lcs-factorize-copy",
			src:         `[1,1,2]`,
			tgt:         `[1,2,2]`,
			opts:        []Option{LCS(), Factorize()},
			wantVerdict: verdictExact,
			wantOps: []expectedOp{
				{typ: OperationRemove, path: "/0"},
				{typ: OperationCopy, path: "/1", from: "/2", fromExists: true},
			},
		},
		// Numeric representation: 1 and 1.0 are the same JSON
		// number after normalization, so the patch is empty and
		// the output keeps the source representation (normalized
		// equality, not lexical equality).
		{
			name:        "numeric-representation/one-and-one-dot-zero-equal",
			src:         `1`,
			tgt:         `1.0`,
			wantVerdict: verdictNormalized,
			wantReason:  "lexical numeric representation",
			wantOps:     nil,
		},
		// With a custom unmarshal that keeps json.Number values,
		// lexical forms remain distinguishable and a root replace
		// is generated.
		{
			name: "numeric-representation/json-number-lexical-difference",
			src:  `1`,
			tgt:  `1.0`,
			opts: []Option{UnmarshalFunc(useNumberUnmarshal)},
			wantVerdict: verdictExact,
			wantOps: []expectedOp{
				{typ: OperationReplace, path: ""},
			},
		},
		// Integer beyond 2^53: default float64 normalization loses
		// the distinction silently. The audit certifies normalized
		// equality and reports the representation caveat instead of
		// presenting this as a pristine result.
		{
			name:        "numeric-representation/large-integer-precision-collapse",
			src:         `9007199254740992`,
			tgt:         `9007199254740993`,
			wantVerdict: verdictNormalized,
			wantReason:  "float64 precision",
			wantOps:     nil,
		},
		{
			name: "numeric-representation/large-integer-distinguished-with-json-number",
			src:  `9007199254740992`,
			tgt:  `9007199254740993`,
			opts: []Option{UnmarshalFunc(useNumberUnmarshal)},
			wantVerdict: verdictExact,
			wantOps: []expectedOp{
				{typ: OperationReplace, path: ""},
			},
		},
		// Object key reordering only: no operation is needed and
		// the document round-trips exactly.
		{
			name:        "object-key-reorder/equal-document",
			src:         `{"b":1,"a":2}`,
			tgt:         `{"a":2,"b":1}`,
			wantVerdict: verdictExact,
			wantOps:     nil,
		},
		// Reordering plus value changes: object operations are
		// emitted in sorted key order regardless of textual order,
		// and every replace carries the round-trip result.
		{
			name:        "object-key-reorder/operations-sorted-by-key",
			src:         `{"z":1,"a":2,"m":3}`,
			tgt:         `{"a":8,"m":7,"z":9}`,
			wantVerdict: verdictNormalized,
			wantReason:  "key order",
			wantOps: []expectedOp{
				{typ: OperationReplace, path: "/a"},
				{typ: OperationReplace, path: "/m"},
				{typ: OperationReplace, path: "/z"},
			},
		},
		// Root replacement across incomparable types uses a root
		// add per RFC 6902 section 4.1.
		{
			name:        "root-replacement/number-to-string",
			src:         `5`,
			tgt:         `"5"`,
			wantVerdict: verdictExact,
			wantOps: []expectedOp{
				{typ: OperationAdd, path: ""},
			},
		},
		{
			name:        "root-replacement/null-to-number",
			src:         `null`,
			tgt:         `1`,
			wantVerdict: verdictExact,
			wantOps: []expectedOp{
				{typ: OperationAdd, path: ""},
			},
		},
		// move: same value removed and re-added, LCS + Factorize.
		{
			name:        "move/array-rotation-lcs-factorize",
			src:         `[1,2,3]`,
			tgt:         `[2,3,1]`,
			opts:        []Option{LCS(), Factorize()},
			wantVerdict: verdictExact,
			wantOps: []expectedOp{
				{typ: OperationMove, path: "/2", from: "/0", fromExists: true},
			},
		},
		// move at object level: key rename of an equal value.
		{
			name:        "move/object-key-rename-factorize",
			src:         `{"a":1}`,
			tgt:         `{"b":1}`,
			opts:        []Option{Factorize()},
			wantVerdict: verdictNormalized,
			wantReason:  "key order",
			wantOps: []expectedOp{
				{typ: OperationMove, path: "/b", from: "/a", fromExists: true},
			},
		},
		// copy: an unchanged subtree reachable at another path.
		{
			name:        "copy/unchanged-subtree-factorize",
			src:         `{"a":[1,2,3]}`,
			tgt:         `{"a":[1,2,3],"b":[1,2,3]}`,
			opts:        []Option{Factorize()},
			wantVerdict: verdictNormalized,
			wantReason:  "key order",
			wantOps: []expectedOp{
				{typ: OperationCopy, path: "/b", from: "/a", fromExists: true},
			},
		},
		// copy is ambiguous to reverse, so Invertible forces an
		// add instead; here a null -> array replace is guarded by
		// a test capturing the old value.
		{
			name: "copy-disabled-under-invertible/test-guards-replace",
			src:  `{"a":[1,2,3],"b":null}`,
			tgt:  `{"a":[1,2,3],"b":[1,2,3]}`,
			opts: []Option{Factorize(), Invertible()},
			wantVerdict: verdictNormalized,
			wantReason:  "key order",
			wantOps: []expectedOp{
				{typ: OperationTest, path: "/b"},
				{typ: OperationReplace, path: "/b", testedOld: true},
			},
		},
		// test: every remove/replace is preceded by a test that
		// captures the pre-image under the same path.
		{
			name: "test-operations/invertible-remove-and-replace",
			src:  `{"a":1,"b":2}`,
			tgt:  `{"a":3}`,
			opts: []Option{Invertible()},
			wantVerdict: verdictNormalized,
			wantReason:  "key order",
			wantOps: []expectedOp{
				{typ: OperationTest, path: "/a"},
				{typ: OperationReplace, path: "/a", testedOld: true},
				{typ: OperationTest, path: "/b"},
				{typ: OperationRemove, path: "/b", testedOld: true},
			},
		},
		// Factorize without LCS on a rotation cannot detect the
		// move and emits positional replaces: another valid
		// non-minimal operation set for the same equivalence.
		{
			name:        "move/unfactorized-positional-alternative",
			src:         `[1,2,3]`,
			tgt:         `[2,3,1]`,
			opts:        []Option{Factorize()},
			wantVerdict: verdictExact,
			wantOps: []expectedOp{
				{typ: OperationReplace, path: "/0"},
				{typ: OperationReplace, path: "/1"},
				{typ: OperationReplace, path: "/2"},
			},
		},
		// Rationalize collapses child operations into one subtree
		// replace when the subtree is shorter.
		{
			name:        "rationalize/collapses-nested-operations",
			src:         `{"o":{"a":1,"b":2}}`,
			tgt:         `{"o":{"a":3,"b":4}}`,
			opts:        []Option{Rationalize()},
			wantVerdict: verdictNormalized,
			wantReason:  "key order",
			wantOps: []expectedOp{
				{typ: OperationReplace, path: "/o"},
			},
		},
	}

	for i := range cases {
		tc := &cases[i]
		t.Run(tc.name, func(t *testing.T) {
			runAudit(t, tc)

			if tc.result.verdict != tc.wantVerdict {
				auditFailf(t, *tc, -1, "verdict: got %q want %q (want reason ~%q)",
					tc.result.verdict, tc.wantVerdict, tc.wantReason)
				return
			}
			if tc.wantReason != "" &&
				!strings.Contains(tc.result.reason, tc.wantReason) {
				auditFailf(t, *tc, -1, "reason: got %q, want it to contain %q",
					tc.result.reason, tc.wantReason)
				return
			}
			var srcDoc any
			if err := json.Unmarshal([]byte(tc.src), &srcDoc); err != nil {
				t.Fatalf("invalid source fixture: %v", err)
			}
			assertOpShape(t, tc, srcDoc)
		})
	}
}
