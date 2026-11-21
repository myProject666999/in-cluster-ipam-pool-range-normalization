/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package poolutil

import (
	"math"
	"math/big"
	"net/netip"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go4.org/netipx"

	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
)

func mustParseRange(s string) netipx.IPRange {
	r, err := netipx.ParseIPRange(s)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return r
}

func expectRanges(actual []netipx.IPRange, expected ...string) {
	GinkgoHelper()
	actualStrings := make([]string, len(actual))
	for i, r := range actual {
		actualStrings[i] = r.String()
	}
	Expect(actualStrings).To(Equal(expected))
}

// rangesAreCanonical asserts that ranges are sorted, disjoint and non-adjacent.
func rangesAreCanonical(ranges []netipx.IPRange) {
	GinkgoHelper()
	for i := 0; i+1 < len(ranges); i++ {
		endOfFirst, startOfNext := ranges[i].To(), ranges[i+1].From()
		Expect(endOfFirst.Compare(startOfNext)).To(Equal(-1), "ranges %v and %v must be disjoint and sorted", ranges[i], ranges[i+1])
		next := endOfFirst.Next()
		Expect(next.IsValid() && next == startOfNext).To(BeFalse(), "ranges %v and %v must not be adjacent", ranges[i], ranges[i+1])
	}
}

var _ = Describe("ParseAddressEntry", func() {
	It("parses a single IP address", func() {
		r, err := ParseAddressEntry("192.168.1.7")
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(Equal(netipx.IPRangeFrom(mustParse("192.168.1.7"), mustParse("192.168.1.7"))))
	})
	It("parses a CIDR prefix and masks host bits", func() {
		r, err := ParseAddressEntry("192.168.1.7/24")
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(Equal(netipx.RangeOfPrefix(netip.MustParsePrefix("192.168.1.0/24"))))
	})
	It("parses a hyphenated range", func() {
		r, err := ParseAddressEntry("192.168.1.7-192.168.1.9")
		Expect(err).NotTo(HaveOccurred())
		Expect(r).To(Equal(mustParseRange("192.168.1.7-192.168.1.9")))
	})
	It("rejects a range whose start is greater than its end", func() {
		_, err := ParseAddressEntry("192.168.1.9-192.168.1.7")
		Expect(err).To(HaveOccurred())
	})
	It("rejects an invalid prefix length", func() {
		_, err := ParseAddressEntry("192.168.1.0/33")
		Expect(err).To(HaveOccurred())
		_, err = ParseAddressEntry("fd01::/129")
		Expect(err).To(HaveOccurred())
	})
	It("rejects garbage", func() {
		_, err := ParseAddressEntry("not-an-address")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("MergeRanges", func() {
	It("merges overlapping ranges", func() {
		merged := MergeRanges([]netipx.IPRange{
			mustParseRange("10.0.0.1-10.0.0.10"),
			mustParseRange("10.0.0.5-10.0.0.15"),
		})
		expectRanges(merged, "10.0.0.1-10.0.0.15")
	})
	It("merges adjacent ranges (to+1 == from)", func() {
		merged := MergeRanges([]netipx.IPRange{
			mustParseRange("10.0.0.1-10.0.0.10"),
			mustParseRange("10.0.0.11-10.0.0.15"),
		})
		expectRanges(merged, "10.0.0.1-10.0.0.15")
	})
	It("merges contained ranges", func() {
		merged := MergeRanges([]netipx.IPRange{
			mustParseRange("10.0.0.1-10.0.0.100"),
			mustParseRange("10.0.0.10-10.0.0.20"),
		})
		expectRanges(merged, "10.0.0.1-10.0.0.100")
	})
	It("merges identical ranges", func() {
		merged := MergeRanges([]netipx.IPRange{
			mustParseRange("10.0.0.1-10.0.0.10"),
			mustParseRange("10.0.0.1-10.0.0.10"),
		})
		expectRanges(merged, "10.0.0.1-10.0.0.10")
	})
	It("keeps disjoint, non-adjacent ranges separate and sorted", func() {
		merged := MergeRanges([]netipx.IPRange{
			mustParseRange("10.0.0.20-10.0.0.30"),
			mustParseRange("10.0.0.1-10.0.0.10"),
		})
		expectRanges(merged, "10.0.0.1-10.0.0.10", "10.0.0.20-10.0.0.30")
		rangesAreCanonical(merged)
	})
	It("is idempotent", func() {
		input := []netipx.IPRange{
			mustParseRange("10.0.0.20-10.0.0.30"),
			mustParseRange("10.0.0.1-10.0.0.10"),
			mustParseRange("10.0.0.11-10.0.0.19"),
			mustParseRange("10.0.0.25-10.0.0.40"),
		}
		once := MergeRanges(input)
		twice := MergeRanges(once)
		expectRanges(once, "10.0.0.1-10.0.0.40")
		Expect(twice).To(Equal(once))
	})
	It("does not merge ranges separated by a single address", func() {
		merged := MergeRanges([]netipx.IPRange{
			mustParseRange("10.0.0.1-10.0.0.10"),
			mustParseRange("10.0.0.12-10.0.0.20"),
		})
		expectRanges(merged, "10.0.0.1-10.0.0.10", "10.0.0.12-10.0.0.20")
	})
	It("merges a range ending at the IPv4 maximum address", func() {
		merged := MergeRanges([]netipx.IPRange{
			mustParseRange("255.255.255.254-255.255.255.255"),
			mustParseRange("255.255.255.253-255.255.255.253"),
		})
		expectRanges(merged, "255.255.255.253-255.255.255.255")
	})
})

var _ = Describe("SubtractRange", func() {
	base := func() []netipx.IPRange {
		return []netipx.IPRange{mustParseRange("10.0.0.10-10.0.0.20")}
	}
	It("splits a range when the middle is excluded", func() {
		out := SubtractRange(base(), mustParseRange("10.0.0.14-10.0.0.16"))
		expectRanges(out, "10.0.0.10-10.0.0.13", "10.0.0.17-10.0.0.20")
		rangesAreCanonical(out)
	})
	It("trims the start of a range", func() {
		out := SubtractRange(base(), mustParseRange("10.0.0.10-10.0.0.12"))
		expectRanges(out, "10.0.0.13-10.0.0.20")
	})
	It("trims the end of a range", func() {
		out := SubtractRange(base(), mustParseRange("10.0.0.18-10.0.0.20"))
		expectRanges(out, "10.0.0.10-10.0.0.17")
	})
	It("deletes a range that is completely covered", func() {
		out := SubtractRange(base(), mustParseRange("10.0.0.0-10.0.0.30"))
		Expect(out).To(BeEmpty())
	})
	It("ignores exclusions outside the pool", func() {
		out := SubtractRange(base(), mustParseRange("10.0.1.0-10.0.1.10"))
		expectRanges(out, "10.0.0.10-10.0.0.20")
		out = SubtractRange(base(), mustParseRange("10.0.0.21-10.0.0.30"))
		expectRanges(out, "10.0.0.10-10.0.0.20")
	})
	It("removes a single address", func() {
		out := SubtractRange(base(), netipx.IPRangeFrom(mustParse("10.0.0.10"), mustParse("10.0.0.10")))
		expectRanges(out, "10.0.0.11-10.0.0.20")
	})
})

var _ = Describe("NormalizePoolSpec", func() {
	DescribeTable("reserved address handling for point-to-point prefix lengths",
		func(addresses []string, prefix int, allocateReserved bool, expectedCount int64, expectedRanges ...string) {
			spec := &v1alpha2.InClusterIPPoolSpec{
				Addresses:                   addresses,
				Prefix:                      prefix,
				AllocateReservedIPAddresses: allocateReserved,
			}
			set, err := NormalizePoolSpec(spec)
			Expect(err).NotTo(HaveOccurred())
			Expect(set.Count().Int64()).To(Equal(expectedCount))
			expectRanges(set.Ranges(), expectedRanges...)
		},
		// Historical behavior, pinned: for IPv4 both the network and the
		// broadcast address of the inferred subnet are removed, even for
		// /31 and /32 where RFC 3021 would allow them.
		Entry("IPv4 /31 removes both addresses", []string{"10.0.0.0/31"}, 31, false, int64(0)),
		Entry("IPv4 /31 with allocateReservedIPAddresses keeps both", []string{"10.0.0.0/31"}, 31, true, int64(2), "10.0.0.0-10.0.0.1"),
		Entry("IPv4 /32 removes the only address", []string{"10.0.0.7/32"}, 32, false, int64(0)),
		Entry("IPv4 /32 with allocateReservedIPAddresses keeps it", []string{"10.0.0.7/32"}, 32, true, int64(1), "10.0.0.7-10.0.0.7"),
		// For IPv6 only the first address (subnet-router anycast) is removed.
		Entry("IPv6 /127 removes only the anycast address", []string{"fd01::/127"}, 127, false, int64(1), "fd01::1-fd01::1"),
		Entry("IPv6 /127 with allocateReservedIPAddresses keeps both", []string{"fd01::/127"}, 127, true, int64(2), "fd01::-fd01::1"),
		Entry("IPv6 /128 removes the only address", []string{"fd01::5/128"}, 128, false, int64(0)),
		Entry("IPv6 /128 with allocateReservedIPAddresses keeps it", []string{"fd01::5/128"}, 128, true, int64(1), "fd01::5-fd01::5"),
	)

	It("rejects an empty address list", func() {
		_, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{Prefix: 24})
		Expect(err).To(HaveOccurred())
	})
	It("rejects mixed IP families", func() {
		_, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses: []string{"10.0.0.1", "fd01::1"},
			Prefix:    24,
		})
		Expect(err).To(MatchError(ContainSubstring("mixed IP families")))
	})
	It("rejects mixed IP families across range and prefix forms", func() {
		_, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses: []string{"10.0.0.1-10.0.0.5", "fd01::/64"},
			Prefix:    24,
		})
		Expect(err).To(MatchError(ContainSubstring("mixed IP families")))
	})
	It("rejects a range with start greater than end", func() {
		_, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses: []string{"10.0.0.10-10.0.0.5"},
			Prefix:    24,
		})
		Expect(err).To(HaveOccurred())
	})
	It("rejects an invalid prefix entry", func() {
		_, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses: []string{"10.0.0.0/33"},
			Prefix:    24,
		})
		Expect(err).To(HaveOccurred())
	})
	It("rejects a spec prefix length that does not fit the address family", func() {
		_, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses: []string{"10.0.0.1"},
			Prefix:    33,
		})
		Expect(err).To(HaveOccurred())
	})
	It("rejects an invalid excluded address entry", func() {
		_, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses:         []string{"10.0.0.0/24"},
			Prefix:            24,
			ExcludedAddresses: []string{"garbage"},
		})
		Expect(err).To(MatchError(ContainSubstring("excluded address entry")))
	})
	It("ignores excluded addresses outside the pool", func() {
		set, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses:         []string{"10.0.0.0/24"},
			Prefix:            24,
			ExcludedAddresses: []string{"10.0.1.0-10.0.1.10"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.Count().Int64()).To(Equal(int64(254)))
	})
	It("subtracts the gateway exactly once even when it is also excluded", func() {
		spec := &v1alpha2.InClusterIPPoolSpec{
			Addresses:         []string{"10.0.0.0/24"},
			Prefix:            24,
			Gateway:           "10.0.0.1",
			ExcludedAddresses: []string{"10.0.0.1"},
		}
		set, err := NormalizePoolSpec(spec)
		Expect(err).NotTo(HaveOccurred())
		Expect(set.Count().Int64()).To(Equal(int64(253)))
		Expect(set.Contains(mustParse("10.0.0.1"))).To(BeFalse())
	})
	It("handles a /8 with small exclusions using few ranges, quickly", func() {
		spec := &v1alpha2.InClusterIPPoolSpec{
			Addresses: []string{"10.0.0.0/8"},
			Prefix:    8,
			Gateway:   "10.0.0.1",
			ExcludedAddresses: []string{
				"10.1.2.3",
				"10.5.0.0/16",
				"10.200.1.1-10.200.1.5",
			},
		}
		start := time.Now()
		set, err := NormalizePoolSpec(spec)
		Expect(err).NotTo(HaveOccurred())
		Expect(time.Since(start)).To(BeNumerically("<", time.Second))
		expectRanges(set.Ranges(),
			"10.0.0.2-10.1.2.2",
			"10.1.2.4-10.4.255.255",
			"10.6.0.0-10.200.1.0",
			"10.200.1.6-10.255.255.254",
		)
		Expect(set.Count().Int64()).To(Equal(int64(16777216 - 2 - 1 - 1 - 65536 - 5)))
		rangesAreCanonical(set.Ranges())
	})
	It("produces identical results for any permutation of the inputs", func() {
		addresses := []string{"10.0.0.10-10.0.0.20", "10.0.0.30/29", "10.0.0.40", "10.0.0.0-10.0.0.5"}
		excluded := []string{"10.0.0.12", "10.0.0.31-10.0.0.32"}
		reference, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses:         addresses,
			Prefix:            24,
			Gateway:           "10.0.0.1",
			ExcludedAddresses: excluded,
		})
		Expect(err).NotTo(HaveOccurred())

		permute := func(in []string, perm []int) []string {
			out := make([]string, len(in))
			for i, p := range perm {
				out[i] = in[p]
			}
			return out
		}
		addressPermutations := [][]int{
			{0, 1, 2, 3}, {3, 2, 1, 0}, {1, 3, 0, 2}, {2, 0, 3, 1}, {3, 0, 1, 2},
		}
		excludedPermutations := [][]int{{0, 1}, {1, 0}}
		for _, ap := range addressPermutations {
			for _, ep := range excludedPermutations {
				set, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
					Addresses:         permute(addresses, ap),
					Prefix:            24,
					Gateway:           "10.0.0.1",
					ExcludedAddresses: permute(excluded, ep),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(set.Equal(reference)).To(BeTrue(), "permutation %v/%v should normalize identically", ap, ep)
			}
		}
	})
	It("is idempotent when its own ranges are normalized again", func() {
		set, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses:         []string{"10.0.0.0/24", "10.0.1.0-10.0.1.10"},
			Prefix:            16,
			ExcludedAddresses: []string{"10.0.0.100-10.0.0.110"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(MergeRanges(set.Ranges())).To(Equal(set.Ranges()))
	})
})

var _ = Describe("AddressSet queries", func() {
	var set *AddressSet
	BeforeEach(func() {
		var err error
		set, err = NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses:         []string{"10.0.0.10-10.0.0.20", "10.0.0.30-10.0.0.32"},
			Prefix:            24,
			ExcludedAddresses: []string{"10.0.0.12-10.0.0.13"},
		})
		Expect(err).NotTo(HaveOccurred())
		// allocatable: 10.0.0.10, 10.0.0.11, 10.0.0.14-10.0.0.20, 10.0.0.30-10.0.0.32
	})

	It("answers Contains via binary search", func() {
		Expect(set.Contains(mustParse("10.0.0.10"))).To(BeTrue())
		Expect(set.Contains(mustParse("10.0.0.11"))).To(BeTrue())
		Expect(set.Contains(mustParse("10.0.0.12"))).To(BeFalse())
		Expect(set.Contains(mustParse("10.0.0.13"))).To(BeFalse())
		Expect(set.Contains(mustParse("10.0.0.14"))).To(BeTrue())
		Expect(set.Contains(mustParse("10.0.0.20"))).To(BeTrue())
		Expect(set.Contains(mustParse("10.0.0.21"))).To(BeFalse())
		Expect(set.Contains(mustParse("10.0.0.30"))).To(BeTrue())
		Expect(set.Contains(mustParse("10.0.0.32"))).To(BeTrue())
		Expect(set.Contains(mustParse("10.0.0.33"))).To(BeFalse())
		Expect(set.Contains(mustParse("10.0.0.9"))).To(BeFalse())
	})

	It("returns the k-th allocatable address", func() {
		expected := []string{
			"10.0.0.10", "10.0.0.11",
			"10.0.0.14", "10.0.0.15", "10.0.0.16", "10.0.0.17", "10.0.0.18", "10.0.0.19", "10.0.0.20",
			"10.0.0.30", "10.0.0.31", "10.0.0.32",
		}
		Expect(set.Count().Int64()).To(Equal(int64(len(expected))))
		for k, want := range expected {
			addr, ok := set.Kth(big.NewInt(int64(k)))
			Expect(ok).To(BeTrue(), "k=%d", k)
			Expect(addr.String()).To(Equal(want), "k=%d", k)
		}
		_, ok := set.Kth(big.NewInt(int64(len(expected))))
		Expect(ok).To(BeFalse())
		_, ok = set.Kth(big.NewInt(-1))
		Expect(ok).To(BeFalse())
	})

	It("enumerates the whole set via Kth consistently with Contains", func() {
		for k := int64(0); k < set.Count().Int64(); k++ {
			addr, ok := set.Kth(big.NewInt(k))
			Expect(ok).To(BeTrue())
			Expect(set.Contains(addr)).To(BeTrue())
		}
	})

	It("explains why an address is unavailable, referencing the responsible input", func() {
		spec := &v1alpha2.InClusterIPPoolSpec{
			Addresses:         []string{"10.0.0.0-10.0.0.10"},
			Prefix:            24,
			Gateway:           "10.0.0.1",
			ExcludedAddresses: []string{"10.0.0.5"},
		}
		s, err := NormalizePoolSpec(spec)
		Expect(err).NotTo(HaveOccurred())

		reason, unavailable := s.ReasonUnavailable(mustParse("10.0.0.1"))
		Expect(unavailable).To(BeTrue())
		Expect(reason).To(ContainSubstring("gateway"))

		reason, unavailable = s.ReasonUnavailable(mustParse("10.0.0.0"))
		Expect(unavailable).To(BeTrue())
		Expect(reason).To(ContainSubstring("network address"))

		reason, unavailable = s.ReasonUnavailable(mustParse("10.0.0.255"))
		Expect(unavailable).To(BeTrue())
		Expect(reason).To(ContainSubstring("broadcast address"))

		reason, unavailable = s.ReasonUnavailable(mustParse("10.0.0.5"))
		Expect(unavailable).To(BeTrue())
		Expect(reason).To(ContainSubstring(`excludedAddresses entry "10.0.0.5"`))

		reason, unavailable = s.ReasonUnavailable(mustParse("10.0.0.100"))
		Expect(unavailable).To(BeTrue())
		Expect(reason).To(ContainSubstring("not contained in the pool addresses"))

		_, unavailable = s.ReasonUnavailable(mustParse("10.0.0.9"))
		Expect(unavailable).To(BeFalse())
	})

	It("converts back to an IPSet", func() {
		ipSet := set.IPSet()
		Expect(ipSet.Contains(mustParse("10.0.0.10"))).To(BeTrue())
		Expect(ipSet.Contains(mustParse("10.0.0.12"))).To(BeFalse())
		Expect(IPSetCount(ipSet)).To(Equal(12))
	})
})

var _ = Describe("AddressSet capacity accounting", func() {
	It("counts the full IPv6 space without overflow", func() {
		set, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses:                   []string{"::/0"},
			Prefix:                      0,
			AllocateReservedIPAddresses: true,
		})
		Expect(err).NotTo(HaveOccurred())
		expected := new(big.Int).Lsh(big.NewInt(1), 128)
		Expect(set.Count().Cmp(expected)).To(Equal(0))
		Expect(set.CountCappedInt()).To(Equal(math.MaxInt))
	})
	It("counts an IPv6 /64 exactly", func() {
		set, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses:                   []string{"fd01::/64"},
			Prefix:                      64,
			AllocateReservedIPAddresses: true,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.Count().Cmp(new(big.Int).Lsh(big.NewInt(1), 64))).To(Equal(0))
		Expect(set.CountCappedInt()).To(Equal(math.MaxInt))
	})
	It("supports Kth at the far end of an IPv6 range", func() {
		set, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses:                   []string{"fd01::/64"},
			Prefix:                      64,
			AllocateReservedIPAddresses: true,
		})
		Expect(err).NotTo(HaveOccurred())
		last, ok := set.Kth(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1)))
		Expect(ok).To(BeTrue())
		Expect(last.String()).To(Equal("fd01::ffff:ffff:ffff:ffff"))
	})
})

var _ = Describe("AddressSet.FindFree", func() {
	It("finds the first free address without scanning", func() {
		set, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses:                   []string{"10.0.0.0/8"},
			Prefix:                      8,
			AllocateReservedIPAddresses: true,
		})
		Expect(err).NotTo(HaveOccurred())

		inUse, err := AddressesToIPSet([]string{"10.0.0.0-10.0.0.9"})
		Expect(err).NotTo(HaveOccurred())

		start := time.Now()
		free, err := set.FindFree(inUse)
		Expect(err).NotTo(HaveOccurred())
		Expect(time.Since(start)).To(BeNumerically("<", time.Second))
		Expect(free.String()).To(Equal("10.0.0.10"))
	})
	It("skips fully used ranges", func() {
		set, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses:                   []string{"10.0.0.10-10.0.0.15", "10.0.0.20-10.0.0.20"},
			Prefix:                      24,
			AllocateReservedIPAddresses: true,
		})
		Expect(err).NotTo(HaveOccurred())
		inUse, err := AddressesToIPSet([]string{"10.0.0.10-10.0.0.15"})
		Expect(err).NotTo(HaveOccurred())
		free, err := set.FindFree(inUse)
		Expect(err).NotTo(HaveOccurred())
		Expect(free.String()).To(Equal("10.0.0.20"))
	})
	It("ignores in-use addresses outside the pool", func() {
		set, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses:                   []string{"10.0.0.10-10.0.0.15"},
			Prefix:                      24,
			AllocateReservedIPAddresses: true,
		})
		Expect(err).NotTo(HaveOccurred())
		inUse, err := AddressesToIPSet([]string{"192.168.0.0/16"})
		Expect(err).NotTo(HaveOccurred())
		free, err := set.FindFree(inUse)
		Expect(err).NotTo(HaveOccurred())
		Expect(free.String()).To(Equal("10.0.0.10"))
	})
	It("returns an error when the pool is exhausted", func() {
		set, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses:                   []string{"10.0.0.10-10.0.0.11"},
			Prefix:                      24,
			AllocateReservedIPAddresses: true,
		})
		Expect(err).NotTo(HaveOccurred())
		inUse, err := AddressesToIPSet([]string{"10.0.0.10-10.0.0.11"})
		Expect(err).NotTo(HaveOccurred())
		_, err = set.FindFree(inUse)
		Expect(err).To(MatchError(ContainSubstring("no address available")))
	})
	It("works in a large IPv6 pool", func() {
		set, err := NormalizePoolSpec(&v1alpha2.InClusterIPPoolSpec{
			Addresses:                   []string{"fd01::/64"},
			Prefix:                      64,
			AllocateReservedIPAddresses: true,
		})
		Expect(err).NotTo(HaveOccurred())
		inUse, err := AddressesToIPSet([]string{"fd01::-fd01::ffff"})
		Expect(err).NotTo(HaveOccurred())
		free, err := set.FindFree(inUse)
		Expect(err).NotTo(HaveOccurred())
		Expect(free.String()).To(Equal("fd01::1:0"))
	})
})

// legacyPoolSpecToIPSet is the pre-normalization implementation of
// PoolSpecToIPSet, kept here to verify the normalized implementation does not
// change the meaning of any previously valid input.
func legacyPoolSpecToIPSet(poolSpec *v1alpha2.InClusterIPPoolSpec) (*netipx.IPSet, error) {
	addressesIPSet, err := AddressesToIPSet(poolSpec.Addresses)
	if err != nil {
		return nil, err
	}

	builder := &netipx.IPSetBuilder{}
	builder.AddSet(addressesIPSet)

	if len(poolSpec.ExcludedAddresses) > 0 {
		excludedAddressesIPSet, err := AddressesToIPSet(poolSpec.ExcludedAddresses)
		if err != nil {
			return nil, err
		}
		builder.RemoveSet(excludedAddressesIPSet)
	}

	if !poolSpec.AllocateReservedIPAddresses {
		subnet := netip.PrefixFrom(addressesIPSet.Ranges()[0].From(), poolSpec.Prefix)
		subnetRange := netipx.RangeOfPrefix(subnet)
		builder.Remove(subnetRange.From())
		if subnet.Addr().Is4() {
			builder.Remove(subnetRange.To())
		}
	}

	if poolSpec.Gateway != "" {
		gateway, err := netip.ParseAddr(poolSpec.Gateway)
		if err != nil {
			return nil, err
		}
		builder.Remove(gateway)
	}

	return builder.IPSet()
}

var _ = Describe("PoolSpecToIPSet compatibility with the legacy implementation", func() {
	specs := map[string]*v1alpha2.InClusterIPPoolSpec{
		"single IP": {
			Addresses: []string{"10.0.0.5"},
			Prefix:    24,
		},
		"range only": {
			Addresses: []string{"10.0.0.10-10.0.0.20"},
			Prefix:    24,
			Gateway:   "10.0.0.1",
		},
		"prefix only": {
			Addresses: []string{"10.0.0.0/24"},
			Prefix:    24,
			Gateway:   "10.0.0.1",
		},
		"mixed forms with overlaps": {
			Addresses: []string{"10.0.0.2", "10.0.0.3-10.0.0.4", "10.0.0.6/31", "10.0.0.4-10.0.0.5"},
			Prefix:    24,
			Gateway:   "10.0.0.1",
		},
		"exclusions splitting ranges": {
			Addresses:         []string{"10.0.0.0/24"},
			Prefix:            24,
			ExcludedAddresses: []string{"10.0.0.5", "10.0.0.10-10.0.0.20", "10.0.0.128/25"},
		},
		"allocateReservedIPAddresses": {
			Addresses:                   []string{"10.0.0.0/24"},
			Prefix:                      24,
			AllocateReservedIPAddresses: true,
			Gateway:                     "10.0.0.1",
		},
		"IPv6 mixed forms": {
			Addresses: []string{"fd01::2", "fd01::3-fd01::4", "fd01::6/127"},
			Prefix:    120,
			Gateway:   "fd01::1",
		},
		"IPv6 with exclusions": {
			Addresses:         []string{"fd01::/120"},
			Prefix:            120,
			ExcludedAddresses: []string{"fd01::10-fd01::1f", "fd01::80"},
		},
		"prefix 0": {
			Addresses: []string{"192.168.0.3-192.168.0.10"},
			Prefix:    0,
			Gateway:   "192.168.0.1",
		},
	}
	for name, spec := range specs {
		spec := spec
		It("produces the same set for "+name, func() {
			legacy, err := legacyPoolSpecToIPSet(spec)
			Expect(err).NotTo(HaveOccurred())
			normalized, err := PoolSpecToIPSet(spec)
			Expect(err).NotTo(HaveOccurred())
			Expect(normalized.Ranges()).To(Equal(legacy.Ranges()))
		})
	}
})

var _ = Describe("Normalized set against a brute-force baseline", func() {
	It("covers exactly the union of inputs minus exclusions in a small space", func() {
		// The whole space under test: 10.0.0.0/24, enumerated address by address.
		spec := &v1alpha2.InClusterIPPoolSpec{
			Addresses: []string{
				"10.0.0.0-10.0.0.5",
				"10.0.0.10-10.0.0.19",
				"10.0.0.30/29", // masks to 10.0.0.24-10.0.0.31
				"10.0.0.40",
				"10.0.0.50-10.0.0.55",
				"10.0.0.55",
			},
			Prefix:  24,
			Gateway: "10.0.0.1",
			ExcludedAddresses: []string{
				"10.0.0.12-10.0.0.13",
				"10.0.0.51",
				"10.0.0.200", // outside the pool: ignored
			},
		}
		set, err := NormalizePoolSpec(spec)
		Expect(err).NotTo(HaveOccurred())

		// Brute-force reference: union of inputs minus gateway, reserved and exclusions.
		contains := func(ranges []netipx.IPRange, ip netip.Addr) bool {
			for _, r := range ranges {
				if r.Contains(ip) {
					return true
				}
			}
			return false
		}
		var inputRanges, excludedRanges []netipx.IPRange
		for _, s := range spec.Addresses {
			r, err := ParseAddressEntry(s)
			Expect(err).NotTo(HaveOccurred())
			inputRanges = append(inputRanges, r)
		}
		for _, s := range spec.ExcludedAddresses {
			r, err := ParseAddressEntry(s)
			Expect(err).NotTo(HaveOccurred())
			excludedRanges = append(excludedRanges, r)
		}

		var expected []netip.Addr
		for i := 0; i < 256; i++ {
			ip := netip.AddrFrom4([4]byte{10, 0, 0, byte(i)})
			inUnion := contains(inputRanges, ip)
			isExcluded := contains(excludedRanges, ip)
			isGateway := ip == mustParse("10.0.0.1")
			isNetwork := ip == mustParse("10.0.0.0")
			isBroadcast := ip == mustParse("10.0.0.255")
			if inUnion && !isExcluded && !isGateway && !isNetwork && !isBroadcast {
				expected = append(expected, ip)
			}
			Expect(set.Contains(ip)).To(Equal(inUnion && !isExcluded && !isGateway && !isNetwork && !isBroadcast),
				"Contains(%s) mismatch", ip)
		}

		// The count matches the enumeration.
		Expect(set.Count().Int64()).To(Equal(int64(len(expected))))

		// Kth enumerates exactly the expected addresses in order.
		for k, want := range expected {
			addr, ok := set.Kth(big.NewInt(int64(k)))
			Expect(ok).To(BeTrue())
			Expect(addr).To(Equal(want), "k=%d", k)
		}

		// The canonical ranges are disjoint and non-adjacent, and their union
		// is exactly the expected set.
		rangesAreCanonical(set.Ranges())
		covered := 0
		for _, r := range set.Ranges() {
			for ip := r.From(); ; ip = ip.Next() {
				Expect(contains([]netipx.IPRange{r}, ip)).To(BeTrue())
				covered++
				if ip == r.To() {
					break
				}
			}
		}
		Expect(covered).To(Equal(len(expected)))
	})
})
