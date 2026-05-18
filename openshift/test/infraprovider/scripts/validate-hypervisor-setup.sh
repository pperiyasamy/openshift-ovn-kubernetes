#!/bin/bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Hypervisor Setup Script
# Run this on the hypervisor to create bridge, container, and VXLAN tunnel
# Requires sudo privileges for network operations

set -e

# Check if sudo is available
if ! command -v sudo &> /dev/null; then
    echo "Error: sudo is required but not found"
    exit 1
fi

# Configuration
NETWORK_NAME="test-net"
BRIDGE_NAME="br-test-${NETWORK_NAME}"
SUBNET_V4="192.168.100.0/24"
GATEWAY_V4="192.168.100.1/24"
CONTAINER_NAME="test-container"
CONTAINER_IP_V4="192.168.100.10/24"
VNI=10001
VXLAN_PORT=5789

# Get hypervisor IP (modify this to match your interface)
HYPERVISOR_IP="${HYPERVISOR_IP:-192.168.111.1}"

# Node IP will be provided as argument
NODE_IP="${1:-}"

if [ -z "$NODE_IP" ]; then
    echo "Usage: $0 <node-ip>"
    echo "Example: $0 192.168.111.20"
    exit 1
fi

echo "========================================="
echo "Hypervisor VXLAN Network Setup"
echo "========================================="
echo "Network: $NETWORK_NAME"
echo "Bridge: $BRIDGE_NAME"
echo "Subnet: $SUBNET_V4"
echo "VNI: $VNI"
echo "VXLAN Port: $VXLAN_PORT"
echo "Hypervisor IP: $HYPERVISOR_IP"
echo "Node IP: $NODE_IP"
echo "========================================="

# Cleanup function
cleanup() {
    echo ""
    echo "Cleaning up previous setup if exists..."
    sudo podman rm -f "$CONTAINER_NAME" 2>/dev/null || true
    sudo ip link del "$BRIDGE_NAME" 2>/dev/null || true
    sudo ip link del "vxlan-node" 2>/dev/null || true
    echo "Cleanup complete."
}

# Run cleanup first
cleanup

echo ""
echo "Step 1: Create Linux bridge"
sudo ip link add "$BRIDGE_NAME" type bridge
sudo ip addr add "$GATEWAY_V4" dev "$BRIDGE_NAME"
sudo ip link set "$BRIDGE_NAME" up
echo "✓ Bridge $BRIDGE_NAME created with gateway $GATEWAY_V4"

echo ""
echo "Step 2: Create test container"
sudo podman run -d --name "$CONTAINER_NAME" --network none registry.access.redhat.com/ubi9/ubi:latest sleep infinity
echo "✓ Container $CONTAINER_NAME created"

# Get container PID
CONTAINER_PID=$(sudo podman inspect -f '{{.State.Pid}}' "$CONTAINER_NAME")
echo "✓ Container PID: $CONTAINER_PID"

echo ""
echo "Step 3: Create veth pair and attach container to bridge"
VETH_HOST="veth-test-h"
VETH_CONT="veth-test-c"

sudo ip link add "$VETH_HOST" type veth peer name "$VETH_CONT"
sudo ip link set "$VETH_HOST" master "$BRIDGE_NAME"
sudo ip link set "$VETH_HOST" up
echo "✓ Created veth pair: $VETH_HOST <-> $VETH_CONT"

echo ""
echo "Step 4: Move veth to container namespace and configure"
sudo ip link set "$VETH_CONT" netns "$CONTAINER_PID"
sudo nsenter -t "$CONTAINER_PID" -n ip addr add "$CONTAINER_IP_V4" dev "$VETH_CONT"
sudo nsenter -t "$CONTAINER_PID" -n ip link set "$VETH_CONT" up
sudo nsenter -t "$CONTAINER_PID" -n ip link set lo up
sudo nsenter -t "$CONTAINER_PID" -n ip route add default via "$(echo $GATEWAY_V4 | cut -d/ -f1)" dev "$VETH_CONT"
echo "✓ Container interface configured with IP $CONTAINER_IP_V4"

echo ""
echo "Step 5: Create VXLAN tunnel to node"
VXLAN_IFACE="vxlan-node"
sudo ip link add "$VXLAN_IFACE" type vxlan id "$VNI" local "$HYPERVISOR_IP" remote "$NODE_IP" dstport "$VXLAN_PORT"
sudo ip link set "$VXLAN_IFACE" master "$BRIDGE_NAME"
sudo ip link set "$VXLAN_IFACE" up
echo "✓ VXLAN tunnel created: $VXLAN_IFACE (VNI $VNI, port $VXLAN_PORT)"

echo ""
echo "========================================="
echo "Hypervisor Setup Complete!"
echo "========================================="
echo ""
echo "Bridge status:"
sudo ip -br link show "$BRIDGE_NAME"
echo ""
echo "Bridge details:"
sudo ip -d link show "$BRIDGE_NAME"
echo ""
echo "Interfaces on bridge:"
sudo ip link show master "$BRIDGE_NAME"
echo ""
echo "Container IP configuration:"
sudo nsenter -t "$CONTAINER_PID" -n ip addr show "$VETH_CONT"
echo ""
echo "Container routing table:"
sudo nsenter -t "$CONTAINER_PID" -n ip route
echo ""
echo "VXLAN interface details:"
sudo ip -d link show "$VXLAN_IFACE"
echo ""
echo "========================================="
echo "Next Steps:"
echo "1. Run validate-node-setup.sh on the cluster node"
echo "2. Run validate-connectivity.sh to test connectivity"
echo "========================================="
echo ""
echo "To cleanup, run: $0 cleanup"
