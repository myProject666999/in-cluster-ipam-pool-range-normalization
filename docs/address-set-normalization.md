# Pool address-set normalization

This note accompanies the introduction of the normalized, canonical
allocatable-address set (`AddressSet`) in `internal/poolutil/addressset.go`.
It records the parsing rules that were preserved, the conclusions on
network/broadcast address handling, and every observable behavior change.

## What changed structurally

All pool inputs — `addresses` (single IPs, CIDR prefixes, hyphenated ranges),
`prefix`, `gateway`, `excludedAddresses` and the reserved-address rules — are
folded by `NormalizePoolSpec` into one canonical `AddressSet`: a sorted list
of disjoint, non-adjacent ranges of a single IP family. Allocation
(`AddressSet.FindFree`), statistics (`Count`/`CountCappedInt`) and validation
(`Contains`, `ReasonUnavailable`) all operate on this single representation;
no code path re-interprets the raw input strings.

- `ParseAddressEntry` is the single parsing routine shared by normalization
  and admission validation.
- `MergeRanges` merges overlapping, contained, identical **and adjacent**
  ranges (adjacent = previous end + 1 == next start). Output is canonical:
  sorted, disjoint, non-adjacent.
- `SubtractRange` subtracts exclusions with interval arithmetic only — no
  per-address expansion, so a `/8` (or an IPv6 `/64`) with a few exclusions
  normalizes in microseconds into a handful of ranges.
- `Contains` and `Kth` (k-th allocatable address) are O(log n) binary
  searches over the canonical ranges plus per-range prefix sums.
- Capacity is computed with `big.Int` and can represent the full IPv6 space
  (2^128) exactly. Integer statistics keep the existing convention: counts
  that do not fit an `int` are reported as `math.MaxInt`.
- Normalization is deterministic: any permutation of the same inputs yields
  an identical set, and re-normalizing a normalized result is a no-op.
  `AddressSet.Equal` compares canonical forms, so pool status updates can
  detect substantive changes.

## Parsing rules that were preserved

- A single IP (`10.0.0.5`), a CIDR prefix (`10.0.0.0/24`, host bits masked
  off), and a hyphenated range (`10.0.0.1-10.0.0.10`) mean exactly what they
  meant before; `PoolSpecToIPSet` is now a thin wrapper over
  `NormalizePoolSpec`, and a test pins its output against a copy of the
  previous implementation for a battery of specs.
- The reserved-address subnet is still derived from the **lowest** pool
  address combined with `spec.prefix` (not from any individual entry), so
  pools whose entries sit inside a larger subnet behave as before.
- IPv4-mapped IPv6 addresses (`::ffff:10.0.0.1`) count as IPv6, as before.
- Ambiguities were resolved in favor of the historical behavior, e.g. an
  unmasked CIDR entry (`10.0.0.7/24`) still denotes the whole masked prefix.

## Network/broadcast address conclusions

Unchanged from the historical behavior, now pinned by tests:

- **IPv4**: the network address (first) and broadcast address (last) of the
  inferred subnet are not allocatable, unless
  `allocateReservedIPAddresses: true`.
- **IPv6**: only the first address of the inferred subnet (the subnet-router
  anycast address) is removed; there is no broadcast removal.
- **IPv4 /31 and /32**: both addresses of a /31, and the single address of a
  /32, are removed — a pool consisting solely of such a prefix is empty
  unless `allocateReservedIPAddresses: true`. This is arguably inconsistent
  with RFC 3021 (point-to-point links have no network/broadcast), but it is
  the long-standing behavior and is preserved. The same applies to **IPv6
  /127 and /128** (RFC 6164): the first address is removed, so a /128 pool is
  empty without the flag. `allocateReservedIPAddresses: true` is the escape
  hatch in all four cases. Changing this default is possible but would be a
  behavior change for existing pools and is deliberately not done here.

## Exclusions and gateway

- Exclusions are subtracted from the merged set: an exclusion in the middle
  of a range splits it in two; one covering a range deletes it; one touching
  an edge trims it.
- **Exclusions outside the pool are silently ignored** (set subtraction is
  naturally a no-op there). This matches the previous behavior; rejecting
  them at admission would be a stricter, incompatible change.
- The gateway is subtracted exactly once. Set subtraction is idempotent, so a
  gateway that is also listed in `excludedAddresses` is not double-counted.
- The normalized set retains each parsed `excludedAddresses` entry, the
  gateway and the reserved addresses for provenance:
  `AddressSet.ReasonUnavailable(ip)` names the responsible input (e.g.
  `excludedAddresses entry "10.0.0.5"`, the pool gateway, the reserved
  network/broadcast address, or "not contained in the pool addresses"). The
  update webhook uses this so a rejected update points at the concrete input
  instead of a bare "pool invalid".

## Admission validation

Pool creation/update validation rejects invalid input up front — ranges
whose ends cross the `spec.prefix` boundary, mixed IPv4/IPv6 families (in
addresses, exclusions, or between addresses and gateway), unparsable
entries, and prefix lengths that do not fit the address family. These checks
now share `ParseAddressEntry` with normalization, so validation and
normalization cannot drift apart. No previously rejected input became valid
and no previously valid input became rejected.

## Observable behavior changes

1. `PoolSpecToIPSet` (and the new `NormalizePoolSpec`) return an **error**
   instead of panicking when `addresses` is empty, and instead of producing
   a corrupt set when families are mixed or `prefix` does not fit the
   address family. All three were already rejected by the webhook, so this
   only hardens the library boundary.
2. `FindFreeAddress` returns the same address as before but finds it by
   binary search over the pool and in-use ranges instead of scanning
   address-by-address; large pools (e.g. a /8 with a used prefix at the
   head) no longer cost one iteration per address.
3. The pool controller no longer contains the dead "subtract the gateway
   again" branch when computing `status.ipAddresses.total` — the gateway was
   already excluded by `PoolSpecToIPSet`, so the branch could never fire.
   Status values are unchanged.
4. The update-validation error message keeps its prefix
   (`pool addresses do not contain allocated addresses: ...`) and now
   appends the per-address reason described above.
5. Adjacent ranges in the normalized output are always merged. `netipx`
   already did this internally, so `Ranges()` output is unchanged in
   practice; the guarantee is now explicit and tested.
