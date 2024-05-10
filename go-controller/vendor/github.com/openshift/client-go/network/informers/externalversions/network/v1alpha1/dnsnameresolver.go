// Code generated by informer-gen. DO NOT EDIT.

package v1alpha1

import (
	"context"
	time "time"

	networkv1alpha1 "github.com/openshift/api/network/v1alpha1"
	versioned "github.com/openshift/client-go/network/clientset/versioned"
	internalinterfaces "github.com/openshift/client-go/network/informers/externalversions/internalinterfaces"
	v1alpha1 "github.com/openshift/client-go/network/listers/network/v1alpha1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	cache "k8s.io/client-go/tools/cache"
)

// DNSNameResolverInformer provides access to a shared informer and lister for
// DNSNameResolvers.
type DNSNameResolverInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() v1alpha1.DNSNameResolverLister
}

type dNSNameResolverInformer struct {
	factory          internalinterfaces.SharedInformerFactory
	tweakListOptions internalinterfaces.TweakListOptionsFunc
	namespace        string
}

// NewDNSNameResolverInformer constructs a new informer for DNSNameResolver type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewDNSNameResolverInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	return NewFilteredDNSNameResolverInformer(client, namespace, resyncPeriod, indexers, nil)
}

// NewFilteredDNSNameResolverInformer constructs a new informer for DNSNameResolver type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredDNSNameResolverInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options v1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.NetworkV1alpha1().DNSNameResolvers(namespace).List(context.TODO(), options)
			},
			WatchFunc: func(options v1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.NetworkV1alpha1().DNSNameResolvers(namespace).Watch(context.TODO(), options)
			},
		},
		&networkv1alpha1.DNSNameResolver{},
		resyncPeriod,
		indexers,
	)
}

func (f *dNSNameResolverInformer) defaultInformer(client versioned.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	return NewFilteredDNSNameResolverInformer(client, f.namespace, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions)
}

func (f *dNSNameResolverInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&networkv1alpha1.DNSNameResolver{}, f.defaultInformer)
}

func (f *dNSNameResolverInformer) Lister() v1alpha1.DNSNameResolverLister {
	return v1alpha1.NewDNSNameResolverLister(f.Informer().GetIndexer())
}
