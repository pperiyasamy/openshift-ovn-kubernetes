package infraprovider

import (
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

var infraProvider api.Provider

// Set infrastructure provider.
func Set(provider api.Provider) {
	infraProvider = provider
}

// Get infrastructure provider.
func Get() api.Provider {
	if infraProvider == nil {
		panic("infra provider not set")
	}
	return infraProvider
}
