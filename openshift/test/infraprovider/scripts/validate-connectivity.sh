#!/bin/bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Connectivity Validation Script
# Tests connectivity between container and node through VXLAN tunnel
# Requires sudo privileges

set -e

# Check if sudo is available
if ! command -v sudo &> /dev/null; then
    echo "Error: sudo is required but not found"
    exit 1
fi

# Configuration
CONTAINER_NAME="test-container"
CONTAINER_IP="192.168.100.10"
NODE_IP="192.168.100.20"
BRIDGE_NAME="br-test-test-net"
VXLAN_IFACE="vxlan-node"
VNI=10001

echo "========================================="
echo "VXLAN Connectivity Validation"
echo "========================================="
echo "Container: $CONTAINER_NAME ($CONTAINER_IP)"
echo "Node: $NODE_IP"
echo "VNI: $VNI"
echo "========================================="

# Get container PID
CONTAINER_PID=$(sudo podman inspect -f '{{.State.Pid}}' "$CONTAINER_NAME" 2>/dev/null || echo "")

if [ -z "$CONTAINER_PID" ]; then
    echo "❌ Error: Container $CONTAINER_NAME not found"
    echo "Run validate-hypervisor-setup.sh first"
    exit 1
fi

echo ""
echo "Test 1: Check hypervisor bridge status"
echo "----------------------------------------"
if sudo ip link show "$BRIDGE_NAME" &>/dev/null; then
    echo "✓ Bridge $BRIDGE_NAME exists"
    sudo ip -br link show "$BRIDGE_NAME"
else
    echo "❌ Bridge $BRIDGE_NAME not found"
    exit 1
fi

echo ""
echo "Test 2: Check VXLAN tunnel on hypervisor"
echo "----------------------------------------"
if sudo ip link show "$VXLAN_IFACE" &>/dev/null; then
    echo "✓ VXLAN interface $VXLAN_IFACE exists"
    sudo ip -d link show "$VXLAN_IFACE" | grep vxlan
else
    echo "❌ VXLAN interface $VXLAN_IFACE not found"
    exit 1
fi

echo ""
echo "Test 3: Check container network configuration"
echo "----------------------------------------"
echo "Container interfaces:"
sudo nsenter -t "$CONTAINER_PID" -n ip -br addr
echo ""
echo "Container routes:"
sudo nsenter -t "$CONTAINER_PID" -n ip route

echo ""
echo "Test 4: Ping from hypervisor to container"
echo "----------------------------------------"
if sudo ping -c 3 -W 2 -I "$BRIDGE_NAME" "$CONTAINER_IP"; then
    echo "✓ Hypervisor can reach container"
else
    echo "❌ Hypervisor cannot reach container"
    exit 1
fi

echo ""
echo "Test 5: Ping from container to hypervisor bridge"
echo "----------------------------------------"
BRIDGE_IP=$(sudo ip addr show "$BRIDGE_NAME" | grep "inet " | awk '{print $2}' | cut -d/ -f1)
if sudo nsenter -t "$CONTAINER_PID" -n ping -c 3 -W 2 "$BRIDGE_IP"; then
    echo "✓ Container can reach hypervisor bridge ($BRIDGE_IP)"
else
    echo "❌ Container cannot reach hypervisor bridge"
    exit 1
fi

echo ""
echo "Test 6: Ping from container to node (via VXLAN)"
echo "----------------------------------------"
echo "Note: This requires node to be set up with validate-node-setup.sh"
if sudo nsenter -t "$CONTAINER_PID" -n ping -c 3 -W 2 "$NODE_IP"; then
    echo "✓ Container can reach node through VXLAN tunnel!"
else
    echo "⚠️  Container cannot reach node"
    echo "   This is expected if node setup is not complete yet"
    echo "   Run validate-node-setup.sh on the cluster node first"
fi

echo ""
echo "Test 7: Check ARP table on hypervisor bridge"
echo "----------------------------------------"
sudo ip neigh show dev "$BRIDGE_NAME"

echo ""
echo "Test 8: Check VXLAN forwarding database"
echo "----------------------------------------"
sudo bridge fdb show dev "$VXLAN_IFACE"

echo ""
echo "========================================="
echo "Validation Summary"
echo "========================================="
echo "✓ Bridge and VXLAN infrastructure is working"
echo "✓ Container networking is functional"
echo "✓ Container can communicate with hypervisor"
echo ""
echo "For full validation:"
echo "1. Run validate-node-setup.sh on cluster node"
echo "2. From node, ping container: ping $CONTAINER_IP"
echo "3. Re-run this script to verify container -> node connectivity"
echo "========================================="
