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
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/netip"
	"slices"
	"sort"
	"strings"

	"go4.org/netipx"

	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
)

// ParseAddressEntry parses a single pool address entry into the inclusive IP
// range it denotes. An entry may be a single IP address ("10.0.0.1"), a CIDR
// prefix ("10.0.0.0/24", host bits are masked off), or a hyphenated range
// ("10.0.0.1-10.0.0.10"). This is the single parsing routine shared by pool
// normalization and admission validation so both interpret user input
// identically.
func ParseAddressEntry(addressStr string) (netipx.IPRange, error) {
	if strings.Contains(addressStr, "-") {
		addrRange, err := netipx.ParseIPRange(addressStr)
		if err != nil {
			return netipx.IPRange{}, err
		}
		return addrRange, nil
	}
	if strings.Contains(addressStr, "/") {
		prefix, err := netip.ParsePrefix(addressStr)
		if err != nil {
			return netipx.IPRange{}, err
		}
		return netipx.RangeOfPrefix(prefix), nil
	}
	addr, err := netip.ParseAddr(addressStr)
	if err != nil {
		return netipx.IPRange{}, err
	}
	return netipx.IPRangeFrom(addr, addr), nil
}

// MergeRanges normalizes a list of IP ranges into the minimal canonical form:
// sorted ascending, overlapping ranges coalesced, and directly adjacent
// ranges (previous.To()+1 == next.From()) merged. The result contains no pair
// of ranges that overlap or touch. The input is not modified. Re-applying
// MergeRanges to its own output is a no-op (idempotent), and the output is
// independent of the input ordering.
func MergeRanges(ranges []netipx.IPRange) []netipx.IPRange {
	if len(ranges) == 0 {
		return nil
	}
	sorted := slices.Clone(ranges)
	slices.SortFunc(sorted, func(a, b netipx.IPRange) int {
		if c := a.From().Compare(b.From()); c != 0 {
			return c
		}
		return a.To().Compare(b.To())
	})

	out := make([]netipx.IPRange, 0, len(sorted))
	for _, r := range sorted {
		if !r.IsValid() {
			continue
		}
		if len(out) == 0 {
			out = append(out, r)
			continue
		}
		last := out[len(out)-1]
		// Merge when r overlaps last, is contained in it, or starts exactly
		// one address after last ends. last.To().Next() is invalid when
		// last.To() is the maximum address of its family; in that case no
		// valid range of the same family can start after it.
		next := last.To().Next()
		adjacentOrBeyond := next.IsValid() && r.From().Compare(next) <= 0
		if r.From().Compare(last.To()) <= 0 || adjacentOrBeyond {
			if r.To().Compare(last.To()) > 0 {
				out[len(out)-1] = netipx.IPRangeFrom(last.From(), r.To())
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// SubtractRange removes ex from every range in ranges. ranges must be in
// canonical form (as produced by MergeRanges). The result is again in
// canonical form: an exclusion in the middle of a range splits it in two, an
// exclusion covering a range deletes it, and an exclusion outside all ranges
// is silently ignored.
func SubtractRange(ranges []netipx.IPRange, ex netipx.IPRange) []netipx.IPRange {
	if !ex.IsValid() {
		return ranges
	}
	out := make([]netipx.IPRange, 0, len(ranges))
	for _, r := range ranges {
		if ex.To().Compare(r.From()) < 0 || ex.From().Compare(r.To()) > 0 {
			out = append(out, r)
			continue
		}
		// r.From() < ex.From() implies ex.From() is not the first address of
		// its family, so Prev() is valid.
		if r.From().Compare(ex.From()) < 0 {
			out = append(out, netipx.IPRangeFrom(r.From(), ex.From().Prev()))
		}
		// Symmetrically, ex.To() < r.To() implies Next() is valid.
		if ex.To().Compare(r.To()) < 0 {
			out = append(out, netipx.IPRangeFrom(ex.To().Next(), r.To()))
		}
	}
	return out
}

// Exclusion records a single parsed excludedAddresses entry so that an
// unavailable address can be traced back to the input that excluded it.
type Exclusion struct {
	// Input is the excludedAddresses entry as written in the pool spec.
	Input string
	// Range is the parsed form of Input.
	Range netipx.IPRange
}

// AddressSet is the normalized, canonical set of allocatable addresses of a
// pool. All pool inputs (addresses, prefix, gateway, excluded addresses and
// the reserved-address rules) are folded into one sorted list of disjoint,
// non-adjacent ranges of a single IP family. Allocation, capacity accounting
// and validation all operate on this single representation and never
// re-interpret the original input forms.
//
// AddressSet is immutable after construction.
type AddressSet struct {
	index *rangeIndex

	// Provenance of addresses that were removed from the raw address union,
	// used to explain why a specific address is not allocatable.
	hasGateway  bool
	gateway     netip.Addr
	hasReserved bool
	networkAddr netip.Addr
	hasBcast    bool
	bcastAddr   netip.Addr
	exclusions  []Exclusion
}

// NormalizePoolSpec folds a pool spec into its canonical AddressSet.
//
// The interpretation of every input form is identical to the historical
// behavior of PoolSpecToIPSet:
//   - addresses may be single IPs, CIDR prefixes (masked) or hyphenated ranges;
//   - unless AllocateReservedIPAddresses is set, the network address (IPv4 and
//     IPv6 anycast) and, for IPv4 only, the broadcast address of the subnet
//     derived from the lowest pool address and spec.Prefix are removed;
//   - the gateway is removed exactly once;
//   - excluded addresses are subtracted from the merged set; exclusions
//     outside the pool are ignored.
//
// Unlike the historical implementation, invalid input (unparsable entries,
// mixed IP families, a prefix length that does not fit the address family, or
// an empty address list) returns an error instead of panicking or producing a
// silently corrupt set. The webhook rejects these inputs at admission time,
// so this error path should not be reachable for persisted pools.
func NormalizePoolSpec(spec *v1alpha2.InClusterIPPoolSpec) (*AddressSet, error) {
	if spec == nil {
		return nil, errors.New("pool spec is nil")
	}
	if len(spec.Addresses) == 0 {
		return nil, errors.New("pool spec must contain at least one address")
	}

	ranges := make([]netipx.IPRange, 0, len(spec.Addresses))
	for _, addressStr := range spec.Addresses {
		r, err := ParseAddressEntry(addressStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse address entry %q: %w", addressStr, err)
		}
		ranges = append(ranges, r)
	}
	if err := validateSingleFamily(ranges); err != nil {
		return nil, fmt.Errorf("invalid addresses: %w", err)
	}
	merged := MergeRanges(ranges)

	set := &AddressSet{}
	working := merged

	if !spec.AllocateReservedIPAddresses {
		// The subnet is derived from the lowest address in the pool, exactly
		// as the historical implementation did.
		subnet := netip.PrefixFrom(merged[0].From(), spec.Prefix)
		if !subnet.IsValid() {
			return nil, fmt.Errorf("prefix length %d is not valid for the address family of the pool addresses", spec.Prefix)
		}
		subnetRange := netipx.RangeOfPrefix(subnet)
		set.hasReserved = true
		set.networkAddr = subnetRange.From()
		working = SubtractRange(working, netipx.IPRangeFrom(subnetRange.From(), subnetRange.From()))
		if subnet.Addr().Is4() {
			set.hasBcast = true
			set.bcastAddr = subnetRange.To()
			working = SubtractRange(working, netipx.IPRangeFrom(subnetRange.To(), subnetRange.To()))
		}
	}

	if spec.Gateway != "" {
		gateway, err := netip.ParseAddr(spec.Gateway)
		if err != nil {
			return nil, fmt.Errorf("failed to parse gateway %q: %w", spec.Gateway, err)
		}
		set.hasGateway = true
		set.gateway = gateway
		working = SubtractRange(working, netipx.IPRangeFrom(gateway, gateway))
	}

	for _, excludedStr := range spec.ExcludedAddresses {
		r, err := ParseAddressEntry(excludedStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse excluded address entry %q: %w", excludedStr, err)
		}
		set.exclusions = append(set.exclusions, Exclusion{Input: excludedStr, Range: r})
		working = SubtractRange(working, r)
	}

	set.index = newRangeIndex(working)
	return set, nil
}

// Ranges returns the canonical ranges of the set: sorted, disjoint and
// non-adjacent. The returned slice is a copy.
func (s *AddressSet) Ranges() []netipx.IPRange {
	return slices.Clone(s.index.ranges)
}

// Contains reports whether ip is an allocatable address of the pool. It runs
// in O(log n) via binary search over the canonical ranges.
func (s *AddressSet) Contains(ip netip.Addr) bool {
	i := s.index.rangeIndexAtOrBefore(ip)
	if i < 0 {
		return false
	}
	return ip.Compare(s.index.ranges[i].To()) <= 0
}

// Kth returns the k-th (zero-based) allocatable address of the set in
// ascending order. It returns false when k is negative or not smaller than
// the total number of allocatable addresses. It runs in O(log n).
func (s *AddressSet) Kth(k *big.Int) (netip.Addr, bool) {
	if k == nil || k.Sign() < 0 || k.Cmp(s.index.total) >= 0 {
		return netip.Addr{}, false
	}
	// First range whose cumulative count (including itself) exceeds k.
	i := sort.Search(len(s.index.ranges), func(i int) bool {
		return s.index.cumulative[i+1].Cmp(k) > 0
	})
	offset := new(big.Int).Sub(k, s.index.cumulative[i])
	return addBig(s.index.ranges[i].From(), offset), true
}

// Count returns the exact number of allocatable addresses. The value can
// represent the full IPv6 address space (2^128) without overflow.
func (s *AddressSet) Count() *big.Int {
	return new(big.Int).Set(s.index.total)
}

// CountCappedInt returns the number of allocatable addresses as an int.
// Counts too large for an int are reported as math.MaxInt, matching the
// historical IPSetCount convention used in pool status.
func (s *AddressSet) CountCappedInt() int {
	return cappedInt(s.index.total)
}

// Equal reports whether s and other describe the same allocatable set.
// Because both sets are canonical, comparing the range lists is sufficient;
// pool status updates can use this to detect substantive changes.
func (s *AddressSet) Equal(other *AddressSet) bool {
	if s == nil || other == nil {
		return s == other
	}
	if len(s.index.ranges) != len(other.index.ranges) {
		return false
	}
	for i, r := range s.index.ranges {
		o := other.index.ranges[i]
		if r.From() != o.From() || r.To() != o.To() {
			return false
		}
	}
	return true
}

// Exclusions returns the parsed excludedAddresses entries, retained for
// provenance (see ReasonUnavailable).
func (s *AddressSet) Exclusions() []Exclusion {
	return slices.Clone(s.exclusions)
}

// ReasonUnavailable explains why ip is not allocatable, referencing the
// specific input responsible (gateway, reserved subnet address, or a concrete
// excludedAddresses entry). The second return value is false when ip is
// allocatable, in which case the reason is empty.
func (s *AddressSet) ReasonUnavailable(ip netip.Addr) (string, bool) {
	if s.Contains(ip) {
		return "", false
	}
	if s.hasGateway && ip == s.gateway {
		return fmt.Sprintf("address %s is the pool gateway", ip), true
	}
	if s.hasReserved && ip == s.networkAddr {
		return fmt.Sprintf("address %s is the reserved network address of the pool subnet", ip), true
	}
	if s.hasBcast && ip == s.bcastAddr {
		return fmt.Sprintf("address %s is the reserved broadcast address of the pool subnet", ip), true
	}
	for _, ex := range s.exclusions {
		if ex.Range.Contains(ip) {
			return fmt.Sprintf("address %s is excluded by excludedAddresses entry %q", ip, ex.Input), true
		}
	}
	return fmt.Sprintf("address %s is not contained in the pool addresses", ip), true
}

// IPSet converts the set to a netipx.IPSet for interop with existing code.
func (s *AddressSet) IPSet() *netipx.IPSet {
	builder := &netipx.IPSetBuilder{}
	for _, r := range s.index.ranges {
		builder.AddRange(r)
	}
	// Cannot fail: the ranges are valid, sorted and disjoint by construction.
	ipSet, _ := builder.IPSet()
	return ipSet
}

// FindFree returns the lowest allocatable address that is not contained in
// inUseIPSet. It uses binary search over the canonical ranges and the in-use
// ranges instead of scanning address by address, so it stays fast even for
// very large pools (e.g. a /8 or an IPv6 /64).
func (s *AddressSet) FindFree(inUseIPSet *netipx.IPSet) (netip.Addr, error) {
	return findFreeAddress(s.index.ranges, inUseIPSet)
}

// findFreeAddress returns the lowest address in ranges (canonical form) that
// is not in inUseIPSet.
func findFreeAddress(ranges []netipx.IPRange, inUseIPSet *netipx.IPSet) (netip.Addr, error) {
	var inUseRanges []netipx.IPRange
	if inUseIPSet != nil {
		inUseRanges = inUseIPSet.Ranges()
	}
	inUse := newRangeIndex(inUseRanges)

	one := big.NewInt(1)
	for _, r := range ranges {
		size := rangeSize(r)
		used := inUse.countWithin(r.From(), r.To())
		if used.Cmp(size) >= 0 {
			continue
		}
		// Binary search the first offset o in [0, size-1] for which the
		// number of free addresses in [r.From(), r.From()+o] is at least 1.
		// That count is non-decreasing in o, and the first offset where it
		// becomes positive is itself a free address.
		lo := big.NewInt(0)
		hi := new(big.Int).Sub(size, one)
		for lo.Cmp(hi) < 0 {
			mid := new(big.Int).Add(lo, hi)
			mid.Rsh(mid, 1)
			ip := addBig(r.From(), mid)
			freeUpTo := new(big.Int).Add(mid, one)
			freeUpTo.Sub(freeUpTo, inUse.countWithin(r.From(), ip))
			if freeUpTo.Sign() > 0 {
				hi = mid
			} else {
				lo = mid.Add(mid, one)
			}
		}
		return addBig(r.From(), lo), nil
	}
	return netip.Addr{}, errors.New("no address available")
}

// validateSingleFamily ensures all ranges belong to one IP family. As in the
// existing webhook validation, IPv4-mapped IPv6 addresses count as IPv6.
func validateSingleFamily(ranges []netipx.IPRange) error {
	if len(ranges) == 0 {
		return nil
	}
	is4 := ranges[0].From().Is4()
	for _, r := range ranges[1:] {
		if r.From().Is4() != is4 || r.To().Is4() != is4 {
			return errors.New("provided addresses are of mixed IP families")
		}
	}
	return nil
}

// rangeIndex indexes a canonical (sorted, disjoint) range list with prefix
// sums of range sizes so that membership, ordinal lookups and address counts
// over any interval are O(log n).
type rangeIndex struct {
	ranges []netipx.IPRange
	// cumulative[i] is the number of addresses covered by ranges[:i].
	cumulative []*big.Int
	total      *big.Int
}

func newRangeIndex(ranges []netipx.IPRange) *rangeIndex {
	idx := &rangeIndex{
		ranges:     ranges,
		cumulative: make([]*big.Int, len(ranges)+1),
		total:      big.NewInt(0),
	}
	idx.cumulative[0] = big.NewInt(0)
	for i, r := range ranges {
		idx.cumulative[i+1] = new(big.Int).Add(idx.cumulative[i], rangeSize(r))
	}
	idx.total = idx.cumulative[len(ranges)]
	return idx
}

// rangeIndexAtOrBefore returns the index of the last range whose From() is
// less than or equal to ip, or -1 if there is none.
func (idx *rangeIndex) rangeIndexAtOrBefore(ip netip.Addr) int {
	return sort.Search(len(idx.ranges), func(i int) bool {
		return idx.ranges[i].From().Compare(ip) > 0
	}) - 1
}

// countUpTo returns the number of indexed addresses less than or equal to ip.
func (idx *rangeIndex) countUpTo(ip netip.Addr) *big.Int {
	i := idx.rangeIndexAtOrBefore(ip)
	if i < 0 {
		return big.NewInt(0)
	}
	r := idx.ranges[i]
	if ip.Compare(r.To()) >= 0 {
		return idx.cumulative[i+1]
	}
	count := new(big.Int).Sub(addrToBig(ip), addrToBig(r.From()))
	count.Add(count, big.NewInt(1))
	return count.Add(count, idx.cumulative[i])
}

// countWithin returns the number of indexed addresses in [lo, hi].
func (idx *rangeIndex) countWithin(lo, hi netip.Addr) *big.Int {
	if lo.Compare(hi) > 0 {
		return big.NewInt(0)
	}
	count := idx.countUpTo(hi)
	if prev := lo.Prev(); prev.IsValid() {
		count.Sub(count, idx.countUpTo(prev))
	}
	return count
}

// rangeSize returns the number of addresses in r (inclusive) as a big.Int.
func rangeSize(r netipx.IPRange) *big.Int {
	size := new(big.Int).Sub(addrToBig(r.To()), addrToBig(r.From()))
	return size.Add(size, big.NewInt(1))
}

// addrToBig converts an IP address to a non-negative big integer.
func addrToBig(ip netip.Addr) *big.Int {
	return new(big.Int).SetBytes(ip.AsSlice())
}

// addBig returns ip + n. The caller must ensure the result stays within the
// address family of ip.
func addBig(ip netip.Addr, n *big.Int) netip.Addr {
	size := 16
	if ip.Is4() {
		size = 4
	}
	sum := new(big.Int).Add(addrToBig(ip), n)
	b := sum.Bytes()
	if len(b) > size {
		return netip.Addr{}
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	addr, ok := netip.AddrFromSlice(out)
	if !ok {
		return netip.Addr{}
	}
	return addr
}

// cappedInt converts a non-negative big.Int to an int, returning math.MaxInt
// when the value does not fit.
func cappedInt(n *big.Int) int {
	if n.IsInt64() && n.Uint64() <= uint64(math.MaxInt) {
		return int(n.Uint64()) //nolint:gosec // [G115] conversion is safe with check beforehand
	}
	return math.MaxInt
}
