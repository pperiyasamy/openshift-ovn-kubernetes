#!/bin/bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Node Setup Script
# Run this on a cluster node (via oc debug) to create VXLAN endpoint
# NOTE: When run via "oc debug node/<node> -- chroot /host", you already have root access

set -e

# Configuration
NETWORK_NAME="test-net"
NODE_IP_V4="192.168.100.20/24"
VNI=10001
VXLAN_PORT=5789

# Hypervisor IP will be provided as argument
HYPERVISOR_IP="${1:-}"

# Get node IP (modify this to match your node's primary interface)
NODE_IP="${NODE_IP:-192.168.111.20}"

if [ -z "$HYPERVISOR_IP" ]; then
    echo "Usage: $0 <hypervisor-ip> [node-primary-ip]"
    echo "Example: $0 192.168.111.1 192.168.111.20"
    exit 1
fi

if [ $# -ge 2 ]; then
    NODE_IP="$2"
fi

echo "========================================="
echo "Node VXLAN Endpoint Setup"
echo "========================================="
echo "Network: $NETWORK_NAME"
echo "Node IP in test network: $NODE_IP_V4"
echo "VNI: $VNI"
echo "VXLAN Port: $VXLAN_PORT"
echo "Node primary IP: $NODE_IP"
echo "Hypervisor IP: $HYPERVISOR_IP"
echo "========================================="

# Cleanup function
cleanup() {
    echo ""
    echo "Cleaning up previous setup if exists..."
    ip link del "vxlan$VNI" 2>/dev/null || true
    echo "Cleanup complete."
}

# Run cleanup first
cleanup

echo ""
echo "Step 1: Create VXLAN interface"
VXLAN_IFACE="vxlan$VNI"
ip link add "$VXLAN_IFACE" type vxlan id "$VNI" local "$NODE_IP" remote "$HYPERVISOR_IP" dstport "$VXLAN_PORT"
echo "✓ VXLAN interface created: $VXLAN_IFACE"

echo ""
echo "Step 2: Configure IP address"
ip addr add "$NODE_IP_V4" dev "$VXLAN_IFACE"
echo "✓ IP address $NODE_IP_V4 configured"

echo ""
echo "Step 3: Bring interface up"
ip link set "$VXLAN_IFACE" up
echo "✓ Interface $VXLAN_IFACE is up"

echo ""
echo "========================================="
echo "Node Setup Complete!"
echo "========================================="
echo ""
echo "VXLAN interface status:"
ip -br addr show "$VXLAN_IFACE"
echo ""
echo "VXLAN interface details:"
ip -d link show "$VXLAN_IFACE"
echo ""
echo "Routing table:"
ip route | grep "$VXLAN_IFACE" || echo "(no specific routes yet)"
echo ""
echo "========================================="
echo "Next Steps:"
echo "1. Test connectivity from hypervisor container to node"
echo "2. Test connectivity from node to hypervisor container"
echo "3. Run validate-connectivity.sh"
echo "========================================="
