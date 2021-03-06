// Code generated by informer-gen. DO NOT EDIT.

package v1alpha1

import (
	internalinterfaces "github.com/kyma-project/kyma/components/service-binding-usage-controller/pkg/client/informers/externalversions/internalinterfaces"
)

// Interface provides access to all the informers in this group version.
type Interface interface {
	// ServiceBindingUsages returns a ServiceBindingUsageInformer.
	ServiceBindingUsages() ServiceBindingUsageInformer
	// UsageKinds returns a UsageKindInformer.
	UsageKinds() UsageKindInformer
}

type version struct {
	factory          internalinterfaces.SharedInformerFactory
	namespace        string
	tweakListOptions internalinterfaces.TweakListOptionsFunc
}

// New returns a new Interface.
func New(f internalinterfaces.SharedInformerFactory, namespace string, tweakListOptions internalinterfaces.TweakListOptionsFunc) Interface {
	return &version{factory: f, namespace: namespace, tweakListOptions: tweakListOptions}
}

// ServiceBindingUsages returns a ServiceBindingUsageInformer.
func (v *version) ServiceBindingUsages() ServiceBindingUsageInformer {
	return &serviceBindingUsageInformer{factory: v.factory, namespace: v.namespace, tweakListOptions: v.tweakListOptions}
}

// UsageKinds returns a UsageKindInformer.
func (v *version) UsageKinds() UsageKindInformer {
	return &usageKindInformer{factory: v.factory, tweakListOptions: v.tweakListOptions}
}
