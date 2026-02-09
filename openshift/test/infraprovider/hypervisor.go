package infraprovider

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/onsi/ginkgo/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/runner"
)

const (
	user = "root"
	port = "22"
)

func hypervisorSshCmdRunner() (api.Runner, error) {
	// Read hypervisor IP from shared directory
	ip, err := readHypervisorIP()
	if err != nil {
		return nil, err
	}
	if ip == "" {
		return nil, nil // Not configured
	}

	// Find SSH key for hypervisor access
	sshKeyPath, err := findSSHKeyPath()
	if err != nil {
		return nil, err
	}
	if sshKeyPath == "" {
		return nil, nil // Not configured
	}

	sshRunner, err := runner.NewSSHRunner(ip, user, port, sshKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create ssh runner for hypervisor: %w", err)
	}

	return sshRunner, nil
}

// readHypervisorIP reads the hypervisor IP from the SHARED_DIR/server-ip file.
// Returns empty string if not configured, error if misconfigured.
func readHypervisorIP() (string, error) {
	sharedDir := os.Getenv("SHARED_DIR")
	if sharedDir == "" {
		return "", nil
	}

	ipFile := filepath.Join(sharedDir, "server-ip")
	exists, err := fileExists(ipFile)
	if err != nil {
		return "", fmt.Errorf("failed to check hypervisor ip file: %w", err)
	}
	if !exists {
		return "", nil
	}

	data, err := os.ReadFile(ipFile)
	if err != nil {
		return "", fmt.Errorf("failed to read hypervisor ip file: %w", err)
	}

	ip := strings.TrimSpace(string(data))
	if ip == "" {
		return "", fmt.Errorf("hypervisor ip file is empty")
	}

	return ip, nil
}

// findSSHKeyPath locates the SSH private key file for hypervisor access.
// Tries equinix-ssh-key first, falls back to packet-ssh-key.
// Returns empty string if not configured, error if misconfigured.
func findSSHKeyPath() (string, error) {
	clusterProfileDir := os.Getenv("CLUSTER_PROFILE_DIR")
	if clusterProfileDir == "" {
		return "", nil
	}

	// Try equinix-ssh-key first
	equinixKey := filepath.Join(clusterProfileDir, "equinix-ssh-key")
	exists, err := fileExists(equinixKey)
	if err != nil {
		return "", fmt.Errorf("failed to check equinix-ssh-key: %w", err)
	}
	if exists {
		return equinixKey, nil
	}

	// Fall back to packet-ssh-key
	packetKey := filepath.Join(clusterProfileDir, "packet-ssh-key")
	exists, err = fileExists(packetKey)
	if err != nil {
		return "", fmt.Errorf("failed to check packet-ssh-key: %w", err)
	}
	if exists {
		return packetKey, nil
	}

	return "", nil
}

// fileExists checks if a file exists and is accessible.
// Returns (false, nil) if file doesn't exist, (false, error) for access errors.
func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

type ipAddressInfo struct {
	Family string `json:"family"`
	Local  string `json:"local"`
}

type linkInfo struct {
	IfName   string          `json:"ifname"`
	Mac      string          `json:"address"`
	AddrInfo []ipAddressInfo `json:"addr_info"`
}

// findInterface retrieves attached interface for the matching subnets from the hypervisor node.
func findInterface(runner api.Runner, v4Subnet, v6Subnet string) (*iface, error) {
	ipAddrCmdArgs := []string{"-j", "addr"}
	result, err := runner.Run("ip", ipAddrCmdArgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve network links: %w", err)
	}

	var links []linkInfo
	if err := json.Unmarshal([]byte(result), &links); err != nil {
		return nil, fmt.Errorf("failed to parse network links: %w", err)
	}

	for _, link := range links {
		if netInfo := tryMatchLink(link, v4Subnet, v6Subnet); netInfo != nil {
			return netInfo, nil
		}
	}
	return nil, fmt.Errorf("no network interface found matching subnets v4=%s v6=%s", v4Subnet, v6Subnet)
}

func tryMatchLink(link linkInfo, v4Subnet, v6Subnet string) *iface {
	net := &iface{}

	for _, addr := range link.AddrInfo {
		// Check for IPv4 match
		if v4Subnet != "" {
			if ok, _ := ipInCIDR(addr.Local, v4Subnet); ok {
				net.v4 = addr.Local
				net.v4Subnet = v4Subnet
				ginkgo.GinkgoLogr.Info("found ip match", "ip4", net.v4, "v4subnet", v4Subnet)
			}
		}

		// Check for IPv6 match
		if v6Subnet != "" {
			if ok, _ := ipInCIDR(addr.Local, v6Subnet); ok {
				net.v6 = addr.Local
				net.v6Subnet = v6Subnet
				ginkgo.GinkgoLogr.Info("found ip match", "ip6", net.v6, "v6subnet", v6Subnet)
			}
		}
	}

	// Only consider this link a match if we found all requested IPs
	hasV4Match := v4Subnet == "" || net.v4 != ""
	hasV6Match := v6Subnet == "" || net.v6 != ""

	if hasV4Match && hasV6Match {
		net.ifName = link.IfName
		net.mac = link.Mac
		ginkgo.GinkgoLogr.Info("found link match", "iface", net.ifName, "v4subnet", v4Subnet, "v6subnet", v6Subnet)
		return net
	}

	// Not a complete match, return nil
	return nil
}

func ipInCIDR(ipStr, cidrStr string) (bool, error) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false, fmt.Errorf("invalid IP address: %q", ipStr)
	}
	_, ipNet, err := net.ParseCIDR(cidrStr)
	if err != nil {
		return false, err
	}
	return ipNet.Contains(ip), nil
}
