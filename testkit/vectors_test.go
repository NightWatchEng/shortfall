package testkit

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/registry"
)

// The conformance-vector tests are the drift fence between
// docs/portability.md and the reference implementation, in both
// directions: the committed vectors are replayed through the Go code
// exactly as a Java or Python port would replay them, so a change to the
// codec, a change to the validator, or a hand-edit of a vector file
// fails here. Regenerate deliberately with
// `go run ./testkit/cmd/genvectors` from the repo root, and update the
// contract in the same PR (ADR-0008).

// portabilityDoc is the specification these vectors are the executable
// half of; the doc-tie test below reads it.
const portabilityDoc = "../docs/portability.md"

func loadVC(t *testing.T) VCVectors {
	t.Helper()
	v, err := LoadVCVectors(filepath.Join(VectorsDir, VCVectorsFile))
	if err != nil {
		t.Fatalf("load biz.vc vectors (regenerate with genvectors from the repo root): %v", err)
	}
	return v
}

func loadRegistryVectors(t *testing.T) RegistryVectors {
	t.Helper()
	v, err := LoadRegistryVectors(filepath.Join(VectorsDir, RegistryVectorsFile))
	if err != nil {
		t.Fatalf("load registry vectors (regenerate with genvectors from the repo root): %v", err)
	}
	return v
}

// TestVCVectorHeader pins the header facts a port reads before any
// vector: they must agree with the Go constants they describe.
func TestVCVectorHeader(t *testing.T) {
	v := loadVC(t)
	if v.MaxEncodedBytes != biz.MaxEncodedBytes {
		t.Errorf("vectors cap %d != biz.MaxEncodedBytes %d", v.MaxEncodedBytes, biz.MaxEncodedBytes)
	}
	if v.MemberKey != biz.MemberKey {
		t.Errorf("vectors member key %q != biz.MemberKey %q", v.MemberKey, biz.MemberKey)
	}
	if len(v.FieldOrder) != 11 {
		t.Fatalf("field order has %d fields, the version-1 wire form has 11: %v", len(v.FieldOrder), v.FieldOrder)
	}
	if v.Delimiter != "|" {
		t.Errorf("delimiter %q, want |", v.Delimiter)
	}
	if len(v.Encode) == 0 {
		t.Fatal("no encode vectors")
	}
	// The codec version is not an exported Go constant; it is observable
	// as the leading field of any canonical encoding.
	enc, err := biz.EncodeVC(v.Encode[0].VC.ValueContext())
	if err != nil {
		t.Fatalf("encode first vector: %v", err)
	}
	if got := strings.SplitN(enc, v.Delimiter, 2)[0]; got != v.CodecVersion {
		t.Errorf("implementation emits version %q, vectors declare %q", got, v.CodecVersion)
	}
}

// TestVCEncodeVectors: every encode vector must produce the committed
// bytes exactly, and those bytes must decode back to the same context.
func TestVCEncodeVectors(t *testing.T) {
	v := loadVC(t)
	for _, vec := range v.Encode {
		t.Run(vec.Name, func(t *testing.T) {
			got, err := biz.EncodeVC(vec.VC.ValueContext())
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if got != vec.Encoded {
				t.Fatalf("encoding drifted\n got %q\nwant %q", got, vec.Encoded)
			}
			if n := len(strings.Split(got, v.Delimiter)); n != len(v.FieldOrder) {
				t.Fatalf("encoding has %d fields, header declares %d", n, len(v.FieldOrder))
			}
			if len(got) > v.MaxEncodedBytes {
				t.Fatalf("encoding is %d bytes, past the %d cap", len(got), v.MaxEncodedBytes)
			}
			back, err := biz.DecodeVC(got)
			if err != nil {
				t.Fatalf("decode of own encoding: %v", err)
			}
			if VCOf(back) != vec.VC {
				t.Fatalf("round trip drifted\n got %+v\nwant %+v", VCOf(back), vec.VC)
			}
		})
	}
}

// TestVCDecodeVectors covers inputs a port will meet on the wire that
// are not themselves canonical encodings: the decoder must accept them,
// yield the committed context, and re-encode to the committed canonical
// form.
func TestVCDecodeVectors(t *testing.T) {
	v := loadVC(t)
	for _, vec := range v.Decode {
		t.Run(vec.Name, func(t *testing.T) {
			got, err := biz.DecodeVC(vec.Encoded)
			if err != nil {
				t.Fatalf("decode %q: %v", vec.Encoded, err)
			}
			if VCOf(got) != vec.VC {
				t.Fatalf("decoded context drifted\n got %+v\nwant %+v", VCOf(got), vec.VC)
			}
			canon, err := biz.EncodeVC(got)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if canon != vec.Canonical {
				t.Fatalf("canonical form drifted\n got %q\nwant %q", canon, vec.Canonical)
			}
			if len(canon) > len(vec.Encoded) {
				t.Fatalf("re-encoding grew (%d > %d bytes) — the size cap would not survive a hop",
					len(canon), len(vec.Encoded))
			}
		})
	}
}

// TestVCRejectVectors: every rejection vector must be rejected, under a
// declared class, with the reference wording the vector recorded.
func TestVCRejectVectors(t *testing.T) {
	v := loadVC(t)
	classes := setOf(VCErrorClasses)
	for _, vec := range v.EncodeReject {
		t.Run("encode/"+vec.Name, func(t *testing.T) {
			if vec.VC == nil {
				t.Fatal("encode rejection vector carries no context")
			}
			_, err := biz.EncodeVC(vec.VC.ValueContext())
			assertRejected(t, err, vec, classes)
		})
	}
	for _, vec := range v.DecodeReject {
		t.Run("decode/"+vec.Name, func(t *testing.T) {
			_, err := biz.DecodeVC(vec.Encoded)
			assertRejected(t, err, vec, classes)
		})
	}
}

func assertRejected(t *testing.T, err error, vec RejectVector, classes map[string]bool) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted an input the contract rejects (class %s)", vec.Error)
	}
	if !classes[vec.Error] {
		t.Fatalf("rejection class %q is not declared", vec.Error)
	}
	if err.Error() != vec.ReferenceMessage {
		t.Fatalf("reference wording drifted\n got %q\nwant %q", err.Error(), vec.ReferenceMessage)
	}
}

// TestVCVectorsAreNotVacuous guards the suite against being quietly
// emptied: a vector file trimmed to nothing would otherwise pass.
func TestVCVectorsAreNotVacuous(t *testing.T) {
	v := loadVC(t)
	if len(v.Encode) < 6 || len(v.Decode) < 4 || len(v.EncodeReject) < 2 || len(v.DecodeReject) < 8 {
		t.Fatalf("vector counts collapsed: encode %d, decode %d, encode_reject %d, decode_reject %d",
			len(v.Encode), len(v.Decode), len(v.EncodeReject), len(v.DecodeReject))
	}
	assertUniqueNames(t, v.Names())
	used := map[string]bool{}
	for _, vec := range append(append([]RejectVector{}, v.EncodeReject...), v.DecodeReject...) {
		used[vec.Error] = true
	}
	for _, class := range VCErrorClasses {
		if !used[class] {
			t.Errorf("declared rejection class %q has no vector — a contract nothing exercises is a wish", class)
		}
	}
}

// TestRegistryAcceptVectors replays every accepted registry document
// through the real validator and compares the facts a port must derive.
func TestRegistryAcceptVectors(t *testing.T) {
	v := loadRegistryVectors(t)
	for _, vec := range v.Accept {
		t.Run(vec.Name, func(t *testing.T) {
			reg, err := registry.Parse([]byte(vec.YAML))
			if err != nil {
				t.Fatalf("valid registry rejected: %v", err)
			}
			got := FactsOf(reg)
			if !got.Equal(vec.Facts) {
				t.Fatalf("derived facts drifted\n got %s\nwant %s", got.JSON(), vec.Facts.JSON())
			}
		})
	}
}

// TestRegistryRejectVectors: every invalid document must be rejected,
// under a declared class, with the reference wording.
func TestRegistryRejectVectors(t *testing.T) {
	v := loadRegistryVectors(t)
	classes := setOf(RegistryErrorClasses)
	for _, vec := range v.Reject {
		t.Run(vec.Name, func(t *testing.T) {
			_, err := registry.Parse([]byte(vec.YAML))
			assertRejected(t, err, vec.RejectVector, classes)
		})
	}
}

// TestRegistryHostVectors pins the propagation allowlist match rule —
// the egress fence of ADR-0003, which every implementation applies to
// the same registry file.
func TestRegistryHostVectors(t *testing.T) {
	v := loadRegistryVectors(t)
	for _, vec := range v.HostAllowlist {
		t.Run(vec.Name, func(t *testing.T) {
			p := registry.Propagation{AllowHosts: vec.AllowHosts}
			if got := p.HostAllowed(vec.Host); got != vec.Allowed {
				t.Fatalf("HostAllowed(%q) = %v, want %v", vec.Host, got, vec.Allowed)
			}
		})
	}
}

// TestRegistryDurationVectors pins the ISO-8601 subset the registry
// speaks; a port that reaches for a full ISO parser accepts durations
// this contract rejects.
func TestRegistryDurationVectors(t *testing.T) {
	v := loadRegistryVectors(t)
	for _, vec := range v.Duration {
		t.Run("accept/"+vec.Name, func(t *testing.T) {
			d, err := registry.ParseISODuration(vec.Input)
			if err != nil {
				t.Fatalf("rejected %q: %v", vec.Input, err)
			}
			if got := int64(d.Seconds()); got != vec.Seconds {
				t.Fatalf("%q = %ds, want %ds", vec.Input, got, vec.Seconds)
			}
		})
	}
	for _, vec := range v.DurationReject {
		t.Run("reject/"+vec.Name, func(t *testing.T) {
			if _, err := registry.ParseISODuration(vec.Input); err == nil {
				t.Fatalf("accepted %q, which the contract rejects", vec.Input)
			}
		})
	}
}

// TestRegistryVectorsAreNotVacuous is the registry half of the
// emptied-file guard.
func TestRegistryVectorsAreNotVacuous(t *testing.T) {
	v := loadRegistryVectors(t)
	if len(v.Accept) < 2 || len(v.Reject) < 20 ||
		len(v.HostAllowlist) < 8 || len(v.Duration) < 4 || len(v.DurationReject) < 6 {
		t.Fatalf("vector counts collapsed: accept %d, reject %d, hosts %d, duration %d/%d",
			len(v.Accept), len(v.Reject), len(v.HostAllowlist), len(v.Duration), len(v.DurationReject))
	}
	assertUniqueNames(t, v.Names())
	// Both host verdicts must appear, or the fence vectors would pass an
	// implementation that answers a constant.
	var allowed, denied int
	for _, h := range v.HostAllowlist {
		if h.Allowed {
			allowed++
		} else {
			denied++
		}
	}
	if allowed == 0 || denied == 0 {
		t.Fatalf("host vectors are one-sided: %d allowed, %d denied", allowed, denied)
	}
	used := map[string]bool{}
	for _, vec := range v.Reject {
		used[vec.Error] = true
	}
	for _, class := range RegistryErrorClasses {
		if !used[class] {
			t.Errorf("declared rejection class %q has no vector", class)
		}
	}
}

// TestPortabilityDocMatchesVectors is the other direction of the fence:
// the specification must name every rejection class the vectors carry
// and every constant they pin. A class added to the vectors without a
// line in the contract fails here.
func TestPortabilityDocMatchesVectors(t *testing.T) {
	raw, err := os.ReadFile(portabilityDoc)
	if err != nil {
		t.Fatalf("read %s: %v", portabilityDoc, err)
	}
	doc := string(raw)
	vc := loadVC(t)
	for _, class := range append(append([]string{}, VCErrorClasses...), RegistryErrorClasses...) {
		if !strings.Contains(doc, "`"+class+"`") {
			t.Errorf("%s does not name rejection class %q", portabilityDoc, class)
		}
	}
	for _, want := range []string{
		biz.MemberKey,
		strconv.Itoa(biz.MaxEncodedBytes),
		strings.Join(vc.FieldOrder, " | "),
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s does not state %q", portabilityDoc, want)
		}
	}
}

func setOf(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

func assertUniqueNames(t *testing.T, names []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" {
			t.Error("a vector has an empty name")
		}
		if seen[n] {
			t.Errorf("vector name %q is used twice — one of them is unreachable in a failure report", n)
		}
		seen[n] = true
	}
}
