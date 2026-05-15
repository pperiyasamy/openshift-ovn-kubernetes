// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package network

import (
	"context"
	"fmt"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// HybridResolver resolves instance names to Instance objects by detecting
// whether the instance is a container or a Kubernetes node.
//
// Resolution strategy:
// 1. Check if instance exists as a Kubernetes node (via API)
// 2. If not a node, check if instance exists as a container
// 3. Return error if instance is not found in either category
type HybridResolver struct {
	containerRunner api.Runner
	nodeRunnerFunc  func(nodeName string) (api.Runner, error)
	kubeClient      kubernetes.Interface
}

// NewHybridResolver creates a new HybridResolver
//
// Parameters:
//   - containerRunner: Runner for executing container runtime commands (podman/docker)
//   - nodeRunnerFunc: Factory function to create a Runner for a specific node
//   - kubeClient: Kubernetes client for node lookups (required for node support)
func NewHybridResolver(
	containerRunner api.Runner,
	nodeRunnerFunc func(nodeName string) (api.Runner, error),
	kubeClient kubernetes.Interface,
) *HybridResolver {
	return &HybridResolver{
		containerRunner: containerRunner,
		nodeRunnerFunc:  nodeRunnerFunc,
		kubeClient:      kubeClient,
	}
}

// Resolve resolves an instance name to an Instance object
func (r *HybridResolver) Resolve(instanceName string) (Instance, error) {
	// First, check if instance exists as a Kubernetes node
	if r.isNode(instanceName) {
		return r.resolveAsNode(instanceName)
	}

	// If not a node, check if instance exists as a container
	if r.isContainer(instanceName) {
		return Instance{
			Type:   InstanceTypeContainer,
			Name:   instanceName,
			Runner: r.containerRunner,
		}, nil
	}

	return Instance{}, fmt.Errorf("instance %q not found as container or node", instanceName)
}

// resolveAsNode creates an Instance for a node
func (r *HybridResolver) resolveAsNode(nodeName string) (Instance, error) {
	if r.nodeRunnerFunc == nil {
		return Instance{}, fmt.Errorf("node runner function not configured")
	}

	runner, err := r.nodeRunnerFunc(nodeName)
	if err != nil {
		return Instance{}, fmt.Errorf("failed to create runner for node %q: %w", nodeName, err)
	}

	return Instance{
		Type:   InstanceTypeNode,
		Name:   nodeName,
		Runner: runner,
	}, nil
}

// isNode checks if the instance exists as a Kubernetes node
func (r *HybridResolver) isNode(name string) bool {
	if r.kubeClient == nil {
		return false
	}

	_, err := r.kubeClient.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	return err == nil
}

// isContainer checks if the instance exists as a container
func (r *HybridResolver) isContainer(name string) bool {
	if r.containerRunner == nil {
		return false
	}

	// Try to inspect the container
	_, err := r.containerRunner.Run("podman", "inspect", name)
	return err == nil
}
