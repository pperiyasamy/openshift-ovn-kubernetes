# VXLAN Network Infrastructure Validation Scripts

These scripts validate the VXLAN-based network approach for extending test networks from the hypervisor to cluster nodes.

## Overview

The validation scripts set up:
1. **Linux bridge** on hypervisor (`br-test-test-net`)
2. **Container** attached to bridge via veth pair
3. **VXLAN tunnel** (VNI 10001, port 5789) between hypervisor and node
4. **VXLAN endpoint** on cluster node

This proves the concept before implementing in Go code.

## Prerequisites

### On Hypervisor:
- SSH access to hypervisor
- `podman` installed
- **sudo privileges** (all scripts use sudo for network operations)
- Know hypervisor's IP address (e.g., `192.168.111.1`)

### On Cluster Node:
- Access via `oc debug node/<node-name>`
- Know node's IP address (e.g., `192.168.111.20`)

### Network Requirements:
- UDP port **5789** must be open between hypervisor and node
- Hypervisor and node can reach each other

## Usage

### Step 1: Setup Hypervisor

Run on the **hypervisor** (requires sudo):

```bash
cd test/infraprovider/scripts
chmod +x *.sh

# Replace NODE_IP with your actual node IP
export NODE_IP=192.168.111.20

# The script will use sudo for network operations
./validate-hypervisor-setup.sh $NODE_IP
```

This script:
- Creates bridge `br-test-test-net` with gateway `192.168.100.1/24`
- Creates container `test-container`
- Attaches container to bridge with IP `192.168.100.10/24`
- Creates VXLAN tunnel to node (VNI 10001, port 5789)

**Expected Output:**
```
✓ Bridge br-test-test-net created with gateway 192.168.100.1/24
✓ Container test-container created
✓ Container PID: 12345
✓ Created veth pair: veth-test-h <-> veth-test-c
✓ Container interface configured with IP 192.168.100.10/24
✓ VXLAN tunnel created: vxlan-node (VNI 10001, port 5789)
```

### Step 2: Setup Node

Run on a **cluster node** via `oc debug`:

```bash
# Get node name
oc get nodes

# Debug into node
oc debug node/<node-name>

# In the debug pod:
chroot /host

# Copy the script (or recreate it)
# Replace HYPERVISOR_IP with your actual hypervisor IP
export HYPERVISOR_IP=192.168.111.1
export NODE_IP=192.168.111.20

bash validate-node-setup.sh $HYPERVISOR_IP $NODE_IP
```

This script:
- Creates VXLAN interface `vxlan10001`
- Configures IP `192.168.100.20/24` on VXLAN interface
- Sets up tunnel to hypervisor (VNI 10001, port 5789)

**Expected Output:**
```
✓ VXLAN interface created: vxlan10001
✓ IP address 192.168.100.20/24 configured
✓ Interface vxlan10001 is up
```

### Step 3: Validate Connectivity

Run on the **hypervisor**:

```bash
./validate-connectivity.sh
```

This script tests:
- Bridge and VXLAN infrastructure
- Container network configuration
- Hypervisor → Container connectivity
- Container → Hypervisor connectivity
- Container → Node connectivity (via VXLAN tunnel)

**Expected Output:**
```
✓ Bridge br-test-test-net exists
✓ VXLAN interface vxlan-node exists
✓ Hypervisor can reach container
✓ Container can reach hypervisor bridge
✓ Container can reach node through VXLAN tunnel!
```

### Step 4: Manual Testing

#### From Container to Node:

```bash
# On hypervisor, exec into container
CONTAINER_PID=$(sudo podman inspect -f '{{.State.Pid}}' test-container)
sudo nsenter -t $CONTAINER_PID -n bash

# Inside container namespace
ping 192.168.100.20  # Node IP in test network
```

#### From Node to Container:

```bash
# On node (via oc debug)
ping 192.168.100.10  # Container IP
```

#### Check VXLAN Statistics:

```bash
# On hypervisor
sudo ip -s link show vxlan-node

# On node (via oc debug, already has root)
ip -s link show vxlan10001
```

### Step 5: Cleanup

On **hypervisor**:
```bash
./cleanup-all.sh
```

On **node** (via `oc debug`):
```bash
ip link del vxlan10001
```

## Network Topology

```
┌─────────────────────────────────────────────────────┐
│ Hypervisor (192.168.111.1)                          │
│                                                     │
│  ┌──────────────────┐                              │
│  │ br-test-test-net │ (192.168.100.1/24)           │
│  │  Linux Bridge    │                              │
│  └────┬─────────┬───┘                              │
│       │         │                                   │
│  ┌────▼───┐ ┌──▼────────┐                          │
│  │ veth   │ │ vxlan-node│                          │
│  │ pair   │ │ VNI 10001 │ ─────┐                   │
│  └────┬───┘ │ port 5789 │      │ VXLAN Tunnel      │
│       │     └───────────┘      │ UDP 5789          │
│  ┌────▼──────────────┐         │                   │
│  │  test-container   │         │                   │
│  │ 192.168.100.10/24 │         │                   │
│  └───────────────────┘         │                   │
└─────────────────────────────────┼───────────────────┘
                                  │
                                  │ Encapsulated
                                  │ L2 over UDP
                                  │
┌─────────────────────────────────▼───────────────────┐
│ Cluster Node (192.168.111.20)                       │
│                                                     │
│  ┌──────────────┐                                  │
│  │ vxlan10001   │                                  │
│  │ VNI 10001    │                                  │
│  │ port 5789    │                                  │
│  │              │                                  │
│  │ 192.168.100.20/24                               │
│  └──────────────┘                                  │
│                                                     │
└─────────────────────────────────────────────────────┘
```

## Configuration Details

| Parameter | Value | Notes |
|-----------|-------|-------|
| Network Name | `test-net` | Test network identifier |
| Bridge Name | `br-test-test-net` | Linux bridge on hypervisor |
| Subnet | `192.168.100.0/24` | Test network CIDR |
| Gateway | `192.168.100.1` | Bridge IP (hypervisor) |
| Container IP | `192.168.100.10` | Test container address |
| Node IP | `192.168.100.20` | Node endpoint address |
| VNI | `10001` | VXLAN Network Identifier |
| VXLAN Port | `5789` | UDP port (custom, not 4789) |

## Troubleshooting

### Container cannot reach node

**Check VXLAN tunnel status:**
```bash
# On hypervisor
sudo ip -d link show vxlan-node

# Should show:
# vxlan id 10001 remote <node-ip> local <hypervisor-ip> dstport 5789
```

**Check if VXLAN port is open:**
```bash
# On hypervisor
sudo ss -ulnp | grep 5789

# On node (via oc debug, already has root)
ss -ulnp | grep 5789
```

**Check firewall:**
```bash
# Allow UDP 5789
firewall-cmd --add-port=5789/udp
# Or
iptables -A INPUT -p udp --dport 5789 -j ACCEPT
```

### No connectivity after setup

**Check bridge forwarding:**
```bash
sudo bridge fdb show dev vxlan-node
```

**Check ARP table:**
```bash
sudo ip neigh show
```

**Verify routes in container:**
```bash
CONTAINER_PID=$(sudo podman inspect -f '{{.State.Pid}}' test-container)
sudo nsenter -t $CONTAINER_PID -n ip route
# Should have default via 192.168.100.1
```

### VXLAN interface not creating

**Check kernel support:**
```bash
modprobe vxlan
lsmod | grep vxlan
```

**Check IP conflicts:**
```bash
sudo ip addr show | grep 192.168.100
```

## What This Validates

✅ Linux bridges can replace podman/docker networks
✅ Veth pairs work for container attachment
✅ VXLAN tunnels provide L2 connectivity
✅ Custom port 5789 avoids infrastructure conflicts
✅ Container and node can communicate directly
✅ IP allocation approach works
✅ No FRR container needed

## Next Steps

After successful validation:
1. Implement in Go code (commits 3-5)
2. Add IP allocation with file-based locking
3. Integrate with baremetal infrastructure provider
4. Create integration tests
