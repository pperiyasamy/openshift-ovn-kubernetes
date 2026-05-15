// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package network

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"strings"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	containernetwork "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/container/network"
	"k8s.io/kubernetes/test/e2e/framework"
	utilnet "k8s.io/utils/net"
)

const (
	// VXLANPort defines the UDP destination port for VXLAN tunnels.
	// We use 5789 instead of the standard 4789 to avoid conflicts with:
	// - OVN overlay network VXLAN tunnels
	// - OpenShift SDN VXLAN tunnels
	// - Any infrastructure VXLAN overlays
	// - Other test network instances
	VXLANPort = 5789

	// VXLANMinVNI is the minimum VNI for test networks (avoid 0 and reserved ranges)
	VXLANMinVNI = 10000

	// VXLANMaxVNI is the maximum VNI for test networks (VXLAN supports up to 16777215)
	VXLANMaxVNI = 16777215

	// bridgePrefix is the prefix for Linux bridge names
	bridgePrefix = "br-test-"

	// vethPrefix is the prefix for veth interface names
	vethPrefix = "veth-"

	// maxInterfaceNameLength is the maximum length for Linux interface names
	maxInterfaceNameLength = 15
)

// VXLANBackend implements the Backend interface using Linux bridges and VXLAN tunnels
type VXLANBackend struct {
	hypervisorRunner api.Runner
	resolver         InstanceResolver
	networks         map[string]*VXLANNetworkState
}

// VXLANNetworkState tracks the state of a VXLAN network
type VXLANNetworkState struct {
	Network     *containernetwork.ContainerEngineNetwork
	BridgeName  string
	VNI         uint32
	IPv4Subnet  string
	IPv6Subnet  string
	IPv4Gateway string
	IPv6Gateway string
	NextIPv4    int // For IP allocation (starts at 10)
	NextIPv6    int // For IP allocation (starts at 10)
	Attachments map[string]*VXLANAttachment
}

// VXLANAttachment tracks an instance's attachment to a network
type VXLANAttachment struct {
	InstanceType   InstanceType
	InterfaceName  string
	IPv4           string
	IPv6           string
	VXLANInterface string // For node attachments (hypervisor side)
}

// NewVXLANBackend creates a new VXLAN-based network backend
func NewVXLANBackend(hypervisorRunner api.Runner, resolver InstanceResolver) *VXLANBackend {
	return &VXLANBackend{
		hypervisorRunner: hypervisorRunner,
		resolver:         resolver,
		networks:         make(map[string]*VXLANNetworkState),
	}
}

// CreateNetwork creates a new network with Linux bridge on the hypervisor
func (v *VXLANBackend) CreateNetwork(name string, subnets ...string) (api.Network, error) {
	if _, exists := v.networks[name]; exists {
		return nil, fmt.Errorf("network %s already exists", name)
	}

	// Generate VNI from network name
	vni := v.generateVNI(name)

	// Create Linux bridge on hypervisor
	bridgeName := fmt.Sprintf("%s%s", bridgePrefix, truncate(name, maxInterfaceNameLength-len(bridgePrefix)))

	// Parse subnets
	var ipv4Subnet, ipv6Subnet string
	var ipv4Gateway, ipv6Gateway string
	for _, subnet := range subnets {
		if utilnet.IsIPv4CIDRString(subnet) {
			ipv4Subnet = subnet
			ipv4Gateway = getGatewayIP(subnet)
		} else if utilnet.IsIPv6CIDRString(subnet) {
			ipv6Subnet = subnet
			ipv6Gateway = getGatewayIP(subnet)
		}
	}

	if err := v.createBridge(bridgeName, ipv4Gateway, ipv6Gateway); err != nil {
		return nil, fmt.Errorf("failed to create bridge %s: %w", bridgeName, err)
	}

	// Create network config
	netConfig := &containernetwork.ContainerEngineNetwork{
		NetName: name,
		Configs: []containernetwork.ContainerEngineNetworkConfig{},
	}
	for _, subnet := range subnets {
		gw := ""
		if utilnet.IsIPv4CIDRString(subnet) {
			gw = stripCIDR(ipv4Gateway)
		} else {
			gw = stripCIDR(ipv6Gateway)
		}
		netConfig.Configs = append(netConfig.Configs, containernetwork.ContainerEngineNetworkConfig{
			Subnet:  subnet,
			Gateway: gw,
		})
	}

	v.networks[name] = &VXLANNetworkState{
		Network:     netConfig,
		BridgeName:  bridgeName,
		VNI:         vni,
		IPv4Subnet:  ipv4Subnet,
		IPv6Subnet:  ipv6Subnet,
		IPv4Gateway: ipv4Gateway,
		IPv6Gateway: ipv6Gateway,
		NextIPv4:    10, // Start IP allocation from .10
		NextIPv6:    10,
		Attachments: make(map[string]*VXLANAttachment),
	}

	framework.Logf("Created VXLAN network %s with VNI %d, bridge %s, subnets v4=%s v6=%s",
		name, vni, bridgeName, ipv4Subnet, ipv6Subnet)
	return netConfig, nil
}

// DeleteNetwork deletes a network and cleans up all resources
func (v *VXLANBackend) DeleteNetwork(network api.Network) error {
	state, exists := v.networks[network.Name()]
	if !exists {
		return fmt.Errorf("network %s not found", network.Name())
	}

	// Detach all instances first
	for instanceName := range state.Attachments {
		instance, err := v.resolver.Resolve(instanceName)
		if err != nil {
			framework.Logf("Warning: failed to resolve instance %s during cleanup: %v", instanceName, err)
			continue
		}
		if err := v.DetachFromInstance(network, instance); err != nil {
			framework.Logf("Warning: failed to detach %s from network %s: %v", instanceName, network.Name(), err)
		}
	}

	// Delete bridge on hypervisor
	if err := v.deleteBridge(state.BridgeName); err != nil {
		return fmt.Errorf("failed to delete bridge %s: %w", state.BridgeName, err)
	}

	delete(v.networks, network.Name())
	framework.Logf("Deleted VXLAN network %s", network.Name())
	return nil
}

// AttachToInstance attaches a network to an instance (container or node)
func (v *VXLANBackend) AttachToInstance(network api.Network, instance Instance) (api.NetworkInterface, error) {
	state, exists := v.networks[network.Name()]
	if !exists {
		return api.NetworkInterface{}, fmt.Errorf("network %s not found", network.Name())
	}

	if _, attached := state.Attachments[instance.Name]; attached {
		return api.NetworkInterface{}, fmt.Errorf("instance %s already attached to network %s", instance.Name, network.Name())
	}

	var netInterface api.NetworkInterface
	var err error

	switch instance.Type {
	case InstanceTypeContainer:
		netInterface, err = v.attachToContainer(state, instance)
	case InstanceTypeNode:
		// Node attachment will be implemented in the next commit
		return api.NetworkInterface{}, fmt.Errorf("node attachment not yet implemented")
	default:
		return api.NetworkInterface{}, fmt.Errorf("unknown instance type: %v", instance.Type)
	}

	if err != nil {
		return api.NetworkInterface{}, err
	}

	framework.Logf("Attached %s %s to network %s with IP %s/%s",
		instance.Type, instance.Name, network.Name(),
		netInterface.IPv4, netInterface.IPv6)

	return netInterface, nil
}

// DetachFromInstance detaches a network from an instance
func (v *VXLANBackend) DetachFromInstance(network api.Network, instance Instance) error {
	state, exists := v.networks[network.Name()]
	if !exists {
		return nil // Network doesn't exist, nothing to detach
	}

	attachment, attached := state.Attachments[instance.Name]
	if !attached {
		return nil // Not attached, nothing to do
	}

	var err error
	switch instance.Type {
	case InstanceTypeContainer:
		err = v.detachFromContainer(state, instance, attachment)
	case InstanceTypeNode:
		// Node detachment will be implemented in the next commit
		return fmt.Errorf("node detachment not yet implemented")
	default:
		return fmt.Errorf("unknown instance type: %v", instance.Type)
	}

	if err != nil {
		return err
	}

	delete(state.Attachments, instance.Name)
	framework.Logf("Detached %s %s from network %s", instance.Type, instance.Name, network.Name())
	return nil
}

// GetInstanceNetworkInterface retrieves network interface information for an attached instance
func (v *VXLANBackend) GetInstanceNetworkInterface(network api.Network, instance Instance) (api.NetworkInterface, error) {
	state, exists := v.networks[network.Name()]
	if !exists {
		return api.NetworkInterface{}, fmt.Errorf("network %s not found", network.Name())
	}

	attachment, attached := state.Attachments[instance.Name]
	if !attached {
		return api.NetworkInterface{}, fmt.Errorf("instance %s not attached to network %s", instance.Name, network.Name())
	}

	return api.NetworkInterface{
		IPv4:        attachment.IPv4,
		IPv6:        attachment.IPv6,
		IPv4Prefix:  state.IPv4Subnet,
		IPv6Prefix:  state.IPv6Subnet,
		IPv4Gateway: stripCIDR(state.IPv4Gateway),
		IPv6Gateway: stripCIDR(state.IPv6Gateway),
		InfName:     attachment.InterfaceName,
	}, nil
}

// attachToContainer attaches a network to a container using veth pair
func (v *VXLANBackend) attachToContainer(state *VXLANNetworkState, instance Instance) (api.NetworkInterface, error) {
	// Allocate IPs
	ipv4, ipv6 := v.allocateIPs(state)

	// Create veth pair names
	vethHost := fmt.Sprintf("%s%s-h", vethPrefix, truncate(instance.Name, maxInterfaceNameLength-len(vethPrefix)-2))
	vethCont := fmt.Sprintf("%s%s-c", vethPrefix, truncate(instance.Name, maxInterfaceNameLength-len(vethPrefix)-2))

	// Get container PID
	pid, err := v.getContainerPID(instance.Name, instance.Runner)
	if err != nil {
		return api.NetworkInterface{}, fmt.Errorf("failed to get container PID: %w", err)
	}

	// Create veth pair
	if err := v.createVethPair(vethHost, vethCont); err != nil {
		return api.NetworkInterface{}, fmt.Errorf("failed to create veth pair: %w", err)
	}

	// Attach host end to bridge and bring it up
	if err := v.attachVethToBridge(vethHost, state.BridgeName); err != nil {
		v.deleteLink(vethHost) // Cleanup on failure
		return api.NetworkInterface{}, fmt.Errorf("failed to attach veth to bridge: %w", err)
	}

	// Move container end to container namespace
	if err := v.moveVethToNamespace(vethCont, pid); err != nil {
		v.deleteLink(vethHost) // Cleanup on failure
		return api.NetworkInterface{}, fmt.Errorf("failed to move veth to container namespace: %w", err)
	}

	// Configure IP addresses inside container
	if err := v.configureContainerInterface(pid, vethCont, ipv4, ipv6, state.IPv4Gateway, state.IPv6Gateway); err != nil {
		v.deleteLink(vethHost) // Cleanup on failure
		return api.NetworkInterface{}, fmt.Errorf("failed to configure container interface: %w", err)
	}

	// Store attachment info
	state.Attachments[instance.Name] = &VXLANAttachment{
		InstanceType:  InstanceTypeContainer,
		InterfaceName: vethCont,
		IPv4:          stripCIDR(ipv4),
		IPv6:          stripCIDR(ipv6),
	}

	return api.NetworkInterface{
		IPv4:        stripCIDR(ipv4),
		IPv6:        stripCIDR(ipv6),
		IPv4Prefix:  state.IPv4Subnet,
		IPv6Prefix:  state.IPv6Subnet,
		IPv4Gateway: stripCIDR(state.IPv4Gateway),
		IPv6Gateway: stripCIDR(state.IPv6Gateway),
		InfName:     vethCont,
	}, nil
}

// detachFromContainer detaches a network from a container
func (v *VXLANBackend) detachFromContainer(state *VXLANNetworkState, instance Instance, attachment *VXLANAttachment) error {
	// Delete veth pair (automatically removes from bridge and container namespace)
	vethHost := fmt.Sprintf("%s%s-h", vethPrefix, truncate(instance.Name, maxInterfaceNameLength-len(vethPrefix)-2))
	return v.deleteLink(vethHost)
}

// Bridge management functions

func (v *VXLANBackend) createBridge(bridgeName string, ipv4Gateway, ipv6Gateway string) error {
	// Create bridge
	if _, err := v.hypervisorRunner.Run("ip", "link", "add", bridgeName, "type", "bridge"); err != nil {
		return fmt.Errorf("failed to create bridge: %w", err)
	}

	// Add IPv4 gateway if specified
	if ipv4Gateway != "" {
		if _, err := v.hypervisorRunner.Run("ip", "addr", "add", ipv4Gateway, "dev", bridgeName); err != nil {
			v.hypervisorRunner.Run("ip", "link", "del", bridgeName) // Cleanup
			return fmt.Errorf("failed to add IPv4 gateway: %w", err)
		}
	}

	// Add IPv6 gateway if specified
	if ipv6Gateway != "" {
		if _, err := v.hypervisorRunner.Run("ip", "addr", "add", ipv6Gateway, "dev", bridgeName); err != nil {
			v.hypervisorRunner.Run("ip", "link", "del", bridgeName) // Cleanup
			return fmt.Errorf("failed to add IPv6 gateway: %w", err)
		}
	}

	// Bring bridge up
	if _, err := v.hypervisorRunner.Run("ip", "link", "set", bridgeName, "up"); err != nil {
		v.hypervisorRunner.Run("ip", "link", "del", bridgeName) // Cleanup
		return fmt.Errorf("failed to bring bridge up: %w", err)
	}

	return nil
}

func (v *VXLANBackend) deleteBridge(bridgeName string) error {
	_, err := v.hypervisorRunner.Run("ip", "link", "del", bridgeName)
	return err
}

// Veth management functions

func (v *VXLANBackend) createVethPair(vethHost, vethCont string) error {
	_, err := v.hypervisorRunner.Run("ip", "link", "add", vethHost, "type", "veth", "peer", "name", vethCont)
	return err
}

func (v *VXLANBackend) attachVethToBridge(vethHost, bridgeName string) error {
	if _, err := v.hypervisorRunner.Run("ip", "link", "set", vethHost, "master", bridgeName); err != nil {
		return err
	}
	_, err := v.hypervisorRunner.Run("ip", "link", "set", vethHost, "up")
	return err
}

func (v *VXLANBackend) moveVethToNamespace(vethCont, pid string) error {
	_, err := v.hypervisorRunner.Run("ip", "link", "set", vethCont, "netns", pid)
	return err
}

func (v *VXLANBackend) configureContainerInterface(pid, vethCont, ipv4, ipv6, ipv4Gateway, ipv6Gateway string) error {
	// Add IPv4 address
	if ipv4 != "" {
		if _, err := v.hypervisorRunner.Run("nsenter", "-t", pid, "-n", "ip", "addr", "add", ipv4, "dev", vethCont); err != nil {
			return fmt.Errorf("failed to add IPv4 address: %w", err)
		}
	}

	// Add IPv6 address
	if ipv6 != "" {
		if _, err := v.hypervisorRunner.Run("nsenter", "-t", pid, "-n", "ip", "addr", "add", ipv6, "dev", vethCont); err != nil {
			return fmt.Errorf("failed to add IPv6 address: %w", err)
		}
	}

	// Bring interface up
	if _, err := v.hypervisorRunner.Run("nsenter", "-t", pid, "-n", "ip", "link", "set", vethCont, "up"); err != nil {
		return fmt.Errorf("failed to bring interface up: %w", err)
	}

	// Add default route via gateway (only if no default route exists)
	if ipv4Gateway != "" {
		gwIP := stripCIDR(ipv4Gateway)
		v.hypervisorRunner.Run("nsenter", "-t", pid, "-n", "ip", "route", "add", "default", "via", gwIP, "dev", vethCont)
	}
	if ipv6Gateway != "" {
		gwIP := stripCIDR(ipv6Gateway)
		v.hypervisorRunner.Run("nsenter", "-t", pid, "-n", "ip", "-6", "route", "add", "default", "via", gwIP, "dev", vethCont)
	}

	return nil
}

func (v *VXLANBackend) deleteLink(linkName string) error {
	_, err := v.hypervisorRunner.Run("ip", "link", "del", linkName)
	return err
}

// Helper functions

func (v *VXLANBackend) getContainerPID(containerName string, runner api.Runner) (string, error) {
	// Use podman inspect to get PID
	out, err := runner.Run("podman", "inspect", "-f", "{{.State.Pid}}", containerName)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (v *VXLANBackend) generateVNI(networkName string) uint32 {
	hash := sha256.Sum256([]byte(networkName))
	// Use lower 24 bits and ensure it's in valid range
	vni := binary.BigEndian.Uint32(hash[:4]) & 0x00FFFFFF
	// Map to our safe range
	vni = VXLANMinVNI + (vni % (VXLANMaxVNI - VXLANMinVNI))
	return vni
}

func (v *VXLANBackend) allocateIPs(state *VXLANNetworkState) (string, string) {
	var ipv4, ipv6 string
	if state.IPv4Subnet != "" {
		ipv4 = allocateIPFromSubnet(state.IPv4Subnet, state.NextIPv4)
		state.NextIPv4++
	}
	if state.IPv6Subnet != "" {
		ipv6 = allocateIPFromSubnet(state.IPv6Subnet, state.NextIPv6)
		state.NextIPv6++
	}
	return ipv4, ipv6
}

// Utility functions

// getGatewayIP returns the first usable IP in a subnet (with CIDR notation)
func getGatewayIP(subnet string) string {
	ip, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return ""
	}

	// Get the first IP in the subnet
	ip = ipNet.IP
	// Increment by 1 to get first usable IP
	ip = incrementIP(ip)

	ones, _ := ipNet.Mask.Size()
	return fmt.Sprintf("%s/%d", ip.String(), ones)
}

// allocateIPFromSubnet allocates an IP from a subnet at the given index
func allocateIPFromSubnet(subnet string, index int) string {
	ip, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return ""
	}

	// Start from network address and add index
	ip = ipNet.IP
	for i := 0; i <= index; i++ {
		ip = incrementIP(ip)
	}

	ones, _ := ipNet.Mask.Size()
	return fmt.Sprintf("%s/%d", ip.String(), ones)
}

// incrementIP increments an IP address by 1
func incrementIP(ip net.IP) net.IP {
	result := make(net.IP, len(ip))
	copy(result, ip)

	for i := len(result) - 1; i >= 0; i-- {
		result[i]++
		if result[i] != 0 {
			break
		}
	}
	return result
}

// stripCIDR removes the CIDR suffix from an IP address string
func stripCIDR(ipWithCIDR string) string {
	if ipWithCIDR == "" {
		return ""
	}
	parts := strings.Split(ipWithCIDR, "/")
	return parts[0]
}

// truncate truncates a string to maxLen characters
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
