// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	v1 "latere.ai/x/cella/manifest/v1"
)

// The quantity forms the fuzzer starts from: the field table's examples, the
// two suffix families, the exponent form, the signs, and the shapes that are
// refused. A seed that is refused is as useful as one that is accepted,
// because the rule below is about what an accepted value means.
var quantitySeeds = []string{
	"1", "2", "500m", "100n", "1500u", "2k", "3M", "4G", "5T", "6P", "7E",
	"1Ki", "2Mi", "2Gi", "10Gi", "1Ti", "1Pi", "1Ei",
	"0", "0.5", "1.25", "0.001", "1e3", "2E3", "1.5e-3", "+5", "-5",
	"0.0005Ki", "9223372036854775807", "9.5e18", "0.0001", "1.0000001",
	"", " ", "one", "1 Gi", "1Gi ", "1.", ".5", "1i", "1Zi", "1e", "e3",
	"1e3.5", "--1", "1m1", "0x10", "1G2",
}

// FuzzQuantity holds this package's parser to the Kubernetes one, which is
// the parser the field table of design 003 names as the syntax. The rule is
// one way: an input this parser accepts is one Kubernetes accepts, with the
// same value in milli-units. The converse is not an equality, because this
// parser is a deliberate subset of that syntax:
//
//   - a value finer than a milli-unit is refused, because neither a cgroup
//     nor a volume size can carry it;
//   - a value whose milli-unit form does not fit an int64 is refused, for
//     the same reason.
//
// Both refusals are on inputs Kubernetes accepts, so the fuzzer asserts
// nothing about a refusal beyond the parser not panicking on it.
func FuzzQuantity(f *testing.F) {
	for _, seed := range quantitySeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		ours, err := ParseQuantity(v1.Quantity(in))
		if err != nil {
			return // a refusal is the subset's business, not the comparison's
		}
		theirs, err := resource.ParseQuantity(in)
		if err != nil {
			t.Fatalf("this parser accepted %q as %d milli-units and the Kubernetes parser refused it: %v", in, ours, err)
		}
		if got := theirs.MilliValue(); got != ours {
			t.Fatalf("%q is %d milli-units here and %d to the Kubernetes parser", in, ours, got)
		}
	})
}
