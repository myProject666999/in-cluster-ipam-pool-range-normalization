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

package webhooks

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"go4.org/netipx"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/internal/poolutil"
	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/pkg/types"
)

const (
	// SkipValidateDeleteWebhookAnnotation is an annotation that can be applied
	// to the InClusterIPPool or GlobalInClusterIPPool to skip delete
	// validation. Necessary for clusterctl move to work as expected.
	SkipValidateDeleteWebhookAnnotation = "ipam.cluster.x-k8s.io/skip-validate-delete-webhook"
)

func (webhook *InClusterIPPool) SetupWebhookWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha2.InClusterIPPool{}).
		WithDefaulter(&inClusterIPPoolAdapter{webhook}).
		WithValidator(&inClusterIPPoolAdapter{webhook}).
		Complete(); err != nil {
		return err
	}
	return ctrl.NewWebhookManagedBy(mgr, &v1alpha2.GlobalInClusterIPPool{}).
		WithDefaulter(&globalInClusterIPPoolAdapter{webhook}).
		WithValidator(&globalInClusterIPPoolAdapter{webhook}).
		Complete()
}

// +kubebuilder:webhook:verbs=create;update;delete,path=/validate-ipam-cluster-x-k8s-io-v1alpha2-inclusterippool,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=ipam.cluster.x-k8s.io,resources=inclusterippools,versions=v1alpha2,name=validation.inclusterippool.ipam.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1
// +kubebuilder:webhook:verbs=create;update,path=/mutate-ipam-cluster-x-k8s-io-v1alpha2-inclusterippool,mutating=true,failurePolicy=fail,matchPolicy=Equivalent,groups=ipam.cluster.x-k8s.io,resources=inclusterippools,versions=v1alpha2,name=default.inclusterippool.ipam.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1

// +kubebuilder:webhook:verbs=create;update;delete,path=/validate-ipam-cluster-x-k8s-io-v1alpha2-globalinclusterippool,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=ipam.cluster.x-k8s.io,resources=globalinclusterippools,versions=v1alpha2,name=validation.globalinclusterippool.ipam.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1
// +kubebuilder:webhook:verbs=create;update,path=/mutate-ipam-cluster-x-k8s-io-v1alpha2-globalinclusterippool,mutating=true,failurePolicy=fail,matchPolicy=Equivalent,groups=ipam.cluster.x-k8s.io,resources=globalinclusterippools,versions=v1alpha2,name=default.globalinclusterippool.ipam.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1

// InClusterIPPool implements a validating and defaulting webhook for InClusterIPPool and GlobalInClusterIPPool.
type InClusterIPPool struct {
	Client client.Reader
}

var (
	_ admission.Defaulter[*v1alpha2.InClusterIPPool]       = &inClusterIPPoolAdapter{}
	_ admission.Validator[*v1alpha2.InClusterIPPool]       = &inClusterIPPoolAdapter{}
	_ admission.Defaulter[*v1alpha2.GlobalInClusterIPPool] = &globalInClusterIPPoolAdapter{}
	_ admission.Validator[*v1alpha2.GlobalInClusterIPPool] = &globalInClusterIPPoolAdapter{}
)

// Default satisfies the defaulting webhook interface.
func (webhook *InClusterIPPool) Default(_ context.Context, _ types.GenericInClusterPool) error {
	return nil
}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type.
func (webhook *InClusterIPPool) ValidateCreate(_ context.Context, pool types.GenericInClusterPool) (admission.Warnings, error) {
	return nil, webhook.validate(nil, pool)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type.
func (webhook *InClusterIPPool) ValidateUpdate(ctx context.Context, oldPool, newPool types.GenericInClusterPool) (admission.Warnings, error) {
	err := webhook.validate(oldPool, newPool)
	if err != nil {
		return nil, err
	}

	oldPoolRef := ipamv1.IPPoolReference{
		APIGroup: v1alpha2.GroupVersion.Group,
		Kind:     oldPool.GetObjectKind().GroupVersionKind().Kind,
		Name:     oldPool.GetName(),
	}
	inUseAddresses, err := poolutil.ListAddressesInUse(ctx, webhook.Client, oldPool.GetNamespace(), oldPoolRef)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	inUseBuilder := &netipx.IPSetBuilder{}
	for _, address := range inUseAddresses {
		ip, err := netip.ParseAddr(address.Spec.Address)
		if err != nil {
			// if an address we fetch for the pool is unparsable then it isn't in the pool ranges
			continue
		}
		inUseBuilder.Add(ip)
	}
	newPoolAddressSet, err := poolutil.NormalizePoolSpec(newPool.PoolSpec())
	if err != nil {
		// these addresses are already validated, this shouldn't happen
		return nil, apierrors.NewInternalError(err)
	}

	inUseBuilder.RemoveSet(newPoolAddressSet.IPSet())
	outOfRangeIPSet, err := inUseBuilder.IPSet()
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	if outOfRange := outOfRangeIPSet.Ranges(); len(outOfRange) > 0 {
		// Point each out-of-range address at the specific pool input that
		// makes it unavailable instead of reporting a bare "pool invalid".
		reasons := sets.Set[string]{}
		for _, address := range inUseAddresses {
			ip, err := netip.ParseAddr(address.Spec.Address)
			if err != nil {
				continue
			}
			if reason, unavailable := newPoolAddressSet.ReasonUnavailable(ip); unavailable {
				reasons.Insert(reason)
			}
		}
		return nil, apierrors.NewBadRequest(fmt.Sprintf("pool addresses do not contain allocated addresses: %v (%s)", outOfRange, strings.Join(sets.List(reasons), "; ")))
	}

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type.
func (webhook *InClusterIPPool) ValidateDelete(ctx context.Context, pool types.GenericInClusterPool) (admission.Warnings, error) {
	if _, ok := pool.GetAnnotations()[SkipValidateDeleteWebhookAnnotation]; ok {
		return nil, nil
	}

	poolTypeRef := ipamv1.IPPoolReference{
		APIGroup: pool.GetObjectKind().GroupVersionKind().Group,
		Kind:     pool.GetObjectKind().GroupVersionKind().Kind,
		Name:     pool.GetName(),
	}

	inUseAddresses, err := poolutil.ListAddressesInUse(ctx, webhook.Client, pool.GetNamespace(), poolTypeRef)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	if len(inUseAddresses) > 0 {
		return nil, apierrors.NewBadRequest("Pool has IPAddresses allocated. Cannot delete Pool until all IPAddresses have been removed.")
	}

	return nil, nil
}

func (webhook *InClusterIPPool) validate(_, newPool types.GenericInClusterPool) (reterr error) {
	var allErrs field.ErrorList
	defer func() {
		if len(allErrs) > 0 {
			reterr = apierrors.NewInvalid(v1alpha2.GroupVersion.WithKind(newPool.GetObjectKind().GroupVersionKind().Kind).GroupKind(), newPool.GetName(), allErrs)
		}
	}()

	if len(newPool.PoolSpec().Addresses) == 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "addresses"), newPool.PoolSpec().Addresses, "addresses is required"))
	}

	if newPool.PoolSpec().Prefix < 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "prefix"), newPool.PoolSpec().Addresses, "a valid prefix is required"))
	}

	var hasIPv4Addr, hasIPv6Addr bool
	for _, address := range newPool.PoolSpec().Addresses {
		ipSet, err := poolutil.AddressToIPSet(address)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "addresses"), address, "provided address is not a valid IP, range, nor CIDR"))
			continue
		}
		from := ipSet.Ranges()[0].From()
		hasIPv4Addr = hasIPv4Addr || from.Is4()
		hasIPv6Addr = hasIPv6Addr || from.Is6()
	}
	if hasIPv4Addr && hasIPv6Addr {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "addresses"), newPool.PoolSpec().Addresses, "provided addresses are of mixed IP families"))
	}

	if newPool.PoolSpec().Gateway != "" {
		gateway, err := netip.ParseAddr(newPool.PoolSpec().Gateway)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "gateway"), newPool.PoolSpec().Gateway, err.Error()))
		} else if gateway.Is6() && hasIPv4Addr || gateway.Is4() && hasIPv6Addr {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "gateway"), newPool.PoolSpec().Gateway, "provided gateway and addresses are of mixed IP families"))
		}
	}

	excludedHasIPv4Addr, excludedHasIPv6Addr := false, false
	for _, address := range newPool.PoolSpec().ExcludedAddresses {
		ipSet, err := poolutil.AddressToIPSet(address)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "excludedAddresses"), address, "provided address is not a valid IP, range, nor CIDR"))
			continue
		}
		from := ipSet.Ranges()[0].From()
		excludedHasIPv4Addr = excludedHasIPv4Addr || from.Is4()
		excludedHasIPv6Addr = excludedHasIPv6Addr || from.Is6()
	}

	if excludedHasIPv4Addr && excludedHasIPv6Addr {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "excludedAddresses"), newPool.PoolSpec().ExcludedAddresses, "provided addresses are of mixed IP families"))
	}

	if (hasIPv4Addr && excludedHasIPv6Addr) || (hasIPv6Addr && excludedHasIPv4Addr) {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "excludedAddresses"), newPool.PoolSpec().ExcludedAddresses, "addresses and excluded addresses are of mixed IP families"))
	}

	if newPool.PoolSpec().AddressReuseGracePeriodSeconds != nil &&
		*newPool.PoolSpec().AddressReuseGracePeriodSeconds < 0 {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "addressReuseGracePeriodSeconds"),
			*newPool.PoolSpec().AddressReuseGracePeriodSeconds,
			"addressReuseGracePeriodSeconds must not be negative",
		))
	}

	if len(allErrs) == 0 {
		errs := validateAddressesAreWithinPrefix(newPool.PoolSpec())
		if len(errs) != 0 {
			allErrs = append(allErrs, errs...)
		}
	}

	return //nolint:nakedret
}

func validatePrefix(spec *v1alpha2.InClusterIPPoolSpec) (*netipx.IPSet, field.ErrorList) {
	var errors field.ErrorList

	addressesIPSet, err := poolutil.AddressesToIPSet(spec.Addresses)
	if err != nil {
		// this should not occur, previous validation should have caught problems here.
		errors := append(errors, field.Invalid(field.NewPath("spec", "addresses"), spec.Addresses, err.Error()))
		return &netipx.IPSet{}, errors
	}

	firstIPInAddresses := addressesIPSet.Ranges()[0].From() // safe because of prior validation
	prefix, err := netip.ParsePrefix(fmt.Sprintf("%s/%d", firstIPInAddresses, spec.Prefix))
	if err != nil {
		errors = append(errors, field.Invalid(field.NewPath("spec", "prefix"), spec.Prefix, "provided prefix is not valid"))
		return &netipx.IPSet{}, errors
	}

	builder := netipx.IPSetBuilder{}
	builder.AddPrefix(prefix)
	prefixIPSet, err := builder.IPSet()
	if err != nil {
		// This should not occur, the prefix has been validated. Converting the prefix to an IPSet
		// for it's ContainsRange function.
		errors := append(errors, field.Invalid(field.NewPath("spec", "prefix"), spec.Prefix, err.Error()))
		return &netipx.IPSet{}, errors
	}

	return prefixIPSet, errors
}

func validateAddressesAreWithinPrefix(spec *v1alpha2.InClusterIPPoolSpec) field.ErrorList {
	var errors field.ErrorList

	prefixIPSet, prefixErrs := validatePrefix(spec)
	if len(prefixErrs) > 0 {
		return prefixErrs
	}

	for _, addressStr := range spec.Addresses {
		addressIPSet, err := poolutil.AddressToIPSet(addressStr)
		if err != nil {
			// this should never occur, previous validations will have caught this.
			errors = append(errors, field.Invalid(field.NewPath("spec", "addresses"), addressStr, "provided address is not a valid IP, range, nor CIDR"))
			continue
		}
		// We know that each addressIPSet should be made up of only one range, it came from a single addressStr
		if !prefixIPSet.ContainsRange(addressIPSet.Ranges()[0]) {
			errors = append(errors, field.Invalid(field.NewPath("spec", "addresses"), addressStr, "provided address belongs to a different subnet than others"))
			continue
		}
	}

	return errors
}

type inClusterIPPoolAdapter struct{ webhook *InClusterIPPool }

func (a *inClusterIPPoolAdapter) Default(ctx context.Context, obj *v1alpha2.InClusterIPPool) error {
	return a.webhook.Default(ctx, obj)
}

func (a *inClusterIPPoolAdapter) ValidateCreate(ctx context.Context, obj *v1alpha2.InClusterIPPool) (admission.Warnings, error) {
	return a.webhook.ValidateCreate(ctx, obj)
}

func (a *inClusterIPPoolAdapter) ValidateUpdate(ctx context.Context, oldObj, newObj *v1alpha2.InClusterIPPool) (admission.Warnings, error) {
	return a.webhook.ValidateUpdate(ctx, oldObj, newObj)
}

func (a *inClusterIPPoolAdapter) ValidateDelete(ctx context.Context, obj *v1alpha2.InClusterIPPool) (admission.Warnings, error) {
	return a.webhook.ValidateDelete(ctx, obj)
}

type globalInClusterIPPoolAdapter struct{ webhook *InClusterIPPool }

func (a *globalInClusterIPPoolAdapter) Default(ctx context.Context, obj *v1alpha2.GlobalInClusterIPPool) error {
	return a.webhook.Default(ctx, obj)
}

func (a *globalInClusterIPPoolAdapter) ValidateCreate(ctx context.Context, obj *v1alpha2.GlobalInClusterIPPool) (admission.Warnings, error) {
	return a.webhook.ValidateCreate(ctx, obj)
}

func (a *globalInClusterIPPoolAdapter) ValidateUpdate(ctx context.Context, oldObj, newObj *v1alpha2.GlobalInClusterIPPool) (admission.Warnings, error) {
	return a.webhook.ValidateUpdate(ctx, oldObj, newObj)
}

func (a *globalInClusterIPPoolAdapter) ValidateDelete(ctx context.Context, obj *v1alpha2.GlobalInClusterIPPool) (admission.Warnings, error) {
	return a.webhook.ValidateDelete(ctx, obj)
}
