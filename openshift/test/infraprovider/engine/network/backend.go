// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package network

import (
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

// InstanceType represents the type of network attachment target
type InstanceType int

const (
	InstanceTypeContainer InstanceType = iota
	InstanceTypeNode
)

// String returns the string representation of the instance type
func (t InstanceType) String() string {
	switch t {
	case InstanceTypeContainer:
		return "container"
	case InstanceTypeNode:
		return "node"
	default:
		return "unknown"
	}
}

// Instance represents a network attachment target (container or node)
type Instance struct {
	Type   InstanceType
	Name   string
	Runner api.Runner // For nodes, this is SSH/oc debug runner; for containers, it's container runtime runner
}

// Backend defines the interface for network implementations that support
// both external containers and cluster nodes
type Backend interface {
	// CreateNetwork creates a new network with the given name and subnets
	CreateNetwork(name string, subnets ...string) (api.Network, error)

	// DeleteNetwork deletes the network and cleans up all associated resources
	DeleteNetwork(network api.Network) error

	// AttachToInstance attaches the network to an instance (container or node)
	// and returns the network interface information
	AttachToInstance(network api.Network, instance Instance) (api.NetworkInterface, error)

	// DetachFromInstance detaches the network from an instance
	DetachFromInstance(network api.Network, instance Instance) error

	// GetInstanceNetworkInterface retrieves the network interface information
	// for an instance attached to the network
	GetInstanceNetworkInterface(network api.Network, instance Instance) (api.NetworkInterface, error)
}

// InstanceResolver resolves instance names to Instance objects
// It determines whether an instance name refers to a container or a node
type InstanceResolver interface {
	Resolve(instanceName string) (Instance, error)
}
