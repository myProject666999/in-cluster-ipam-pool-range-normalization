/*
Copyright 2023 The Kubernetes Authors.

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

// Package poolutil implements utility functions to manage pools of IP addresses and network prefixes.
package poolutil

import (
	"context"
	"errors"
	"net/netip"

	"go4.org/netipx"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/internal/index"
)

// AddressesOutOfRangeIPSet returns an IPSet of the inUseAddresses IPs that are
// not in the poolIPSet.
func AddressesOutOfRangeIPSet(inUseAddresses []ipamv1.IPAddress, poolIPSet *netipx.IPSet) (*netipx.IPSet, error) {
	outOfRangeBuilder := &netipx.IPSetBuilder{}
	for _, address := range inUseAddresses {
		ip, err := netip.ParseAddr(address.Spec.Address)
		if err != nil {
			// if an address we fetch for the pool is unparsable then it isn't in the pool ranges
			continue
		}
		outOfRangeBuilder.Add(ip)
	}
	outOfRangeBuilder.RemoveSet(poolIPSet)
	return outOfRangeBuilder.IPSet()
}

// ListAddressesInUse fetches all IPAddresses belonging to the specified pool.
// Note: requires `index.ipAddressByCombinedPoolRef` to be set up.
func ListAddressesInUse(ctx context.Context, c client.Reader, namespace string, poolRef ipamv1.IPPoolReference) ([]ipamv1.IPAddress, error) {
	addresses := &ipamv1.IPAddressList{}
	err := c.List(ctx, addresses,
		client.MatchingFields{
			index.IPAddressPoolRefCombinedField: index.IPPoolRefValue(poolRef),
		},
		client.InNamespace(namespace),
	)
	if err != nil {
		return nil, err
	}
	return addresses.Items, nil
}

// AddressByNamespacedName finds a specific ip address by namespace and name in a slice of addresses.
func AddressByNamespacedName(addresses []ipamv1.IPAddress, namespace, name string) *ipamv1.IPAddress {
	for _, a := range addresses {
		if a.Namespace == namespace && a.Name == name {
			return &a
		}
	}
	return nil
}

// FindFreeAddress returns the next free IP Address in a range based on a set of existing addresses.
// It uses binary search over the pool and in-use ranges rather than scanning
// address by address, so it remains fast for very large pools.
func FindFreeAddress(poolIPSet *netipx.IPSet, inUseIPSet *netipx.IPSet) (netip.Addr, error) {
	if poolIPSet == nil {
		return netip.Addr{}, errors.New("no address available")
	}
	return findFreeAddress(poolIPSet.Ranges(), inUseIPSet)
}

// PoolSpecToIPSet converts a pool spec to an IPSet. Reserved addresses will be
// omitted from the set depending on whether the
// `spec.AllocateReservedIPAddresses` flag is set.
//
// The conversion is a thin wrapper around NormalizePoolSpec: allocation,
// statistics and validation all share the same canonical address set.
func PoolSpecToIPSet(poolSpec *v1alpha2.InClusterIPPoolSpec) (*netipx.IPSet, error) {
	addressSet, err := NormalizePoolSpec(poolSpec)
	if err != nil {
		return nil, err // should not happen, webhook validates pools for correctness.
	}
	return addressSet.IPSet(), nil
}

// AddressesToIPSet converts an array of addresses to an AddressesToIPSet
// addresses may be specified as individual IPs, CIDR ranges, or hyphenated IP
// ranges.
func AddressesToIPSet(addresses []string) (*netipx.IPSet, error) {
	builder := &netipx.IPSetBuilder{}
	for _, addressStr := range addresses {
		ipSet, err := AddressToIPSet(addressStr)
		if err != nil {
			return nil, err
		}
		builder.AddSet(ipSet)
	}
	return builder.IPSet()
}

// AddressToIPSet converts an addresses to an AddressesToIPSet addresses may be
// specified as individual IPs, CIDR ranges, or hyphenated IP ranges.
func AddressToIPSet(addressStr string) (*netipx.IPSet, error) {
	addrRange, err := ParseAddressEntry(addressStr)
	if err != nil {
		return nil, err
	}
	builder := &netipx.IPSetBuilder{}
	builder.AddRange(addrRange)
	return builder.IPSet()
}

// IPSetCount returns the number of IPs contained in the given IPSet.
// This function returns type int, which is much smaller than
// the possible size of a range, which could be 2^128 IPs.
// When an IPSet's count is would be larger than an int, math.MaxInt
// is returned instead.
func IPSetCount(ipSet *netipx.IPSet) int {
	if ipSet == nil {
		return 0
	}
	return cappedInt(newRangeIndex(ipSet.Ranges()).total)
}

// AddressStrParses checks to see that the addresss string is one of
// a valid single IP address, a hyphonated IP range, or a Prefix.
func AddressStrParses(addressStr string) bool {
	_, err := AddressToIPSet(addressStr)
	return err == nil
}
