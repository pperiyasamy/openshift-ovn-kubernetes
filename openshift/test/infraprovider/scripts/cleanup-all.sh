#!/bin/bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Cleanup Script
# Removes all test network infrastructure
# Requires sudo privileges

set -e

# Check if sudo is available
if ! command -v sudo &> /dev/null; then
    echo "Error: sudo is required but not found"
    exit 1
fi

CONTAINER_NAME="test-container"
BRIDGE_NAME="br-test-test-net"
VXLAN_IFACE="vxlan-node"
VNI=10001

echo "========================================="
echo "Cleaning Up Test Infrastructure"
echo "========================================="

# Cleanup on hypervisor
echo ""
echo "Cleaning up hypervisor..."
sudo podman rm -f "$CONTAINER_NAME" 2>/dev/null && echo "✓ Removed container $CONTAINER_NAME" || echo "  Container not found"
sudo ip link del "$BRIDGE_NAME" 2>/dev/null && echo "✓ Removed bridge $BRIDGE_NAME" || echo "  Bridge not found"
sudo ip link del "$VXLAN_IFACE" 2>/dev/null && echo "✓ Removed VXLAN $VXLAN_IFACE" || echo "  VXLAN interface not found"

echo ""
echo "========================================="
echo "Hypervisor Cleanup Complete"
echo "========================================="
echo ""
echo "To cleanup node, run on the node:"
echo "  ip link del vxlan$VNI"
echo "========================================="
