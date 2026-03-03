package infraprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	operatorv1client "github.com/openshift/client-go/operator/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovnkconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/container"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/container/network"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/portalloc"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/runner"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/testcontext"

	"github.com/onsi/ginkgo/v2"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
)

const (
	hypervisorNodeUser      = "root"
	hypervisorSshport       = "22"
	ovnAnnotationNodeIfAddr = "k8s.ovn.org/node-primary-ifaddr"
	ovnpodNamespace         = "openshift-ovn-kubernetes"
	// use network name created for attaching frr container with
	// cluster primary network as per changes in the link:
	// https://github.com/openshift/release/blob/db6697de61f4ae7e05c5a2db782a87c459e849bf/ci-operator/step-registry/baremetalds/e2e/ovn/bgp/pre/baremetalds-e2e-ovn-bgp-pre-commands.sh#L123-L124
	primaryNetworkName         = "ostestbm_net"
	frrContainerPrimaryNetIPv4 = "192.168.111.3"
	frrContainerPrimaryNetIPv6 = "fd2e:6f44:5dd8:c956::3"
	externalFRRContainerName   = "frr"

	// Environment variable names for test configuration
	// These are set during infra provider initialization and consumed by test selection logic
	EnvVarOVNGatewayMode     = "OVN_GATEWAY_MODE"
	EnvVarEVPNFeatureEnabled = "EVPN_FEATURE_ENABLED"
)

// Provider extends the base api.Provider interface with OpenShift-specific
// initialization that uses lazy loading to optimize performance for non-test commands.
//
// The initialization is split into two phases:
//  1. New() and LoadTestConfigs() performs lightweight initialization - loads cluster
//     capabilities (EVPN, gateway mode) needed for test filtering in 'list' command.
//     This phase MUST avoid verbose logging (e.g., from RunHostCmd/builder.go)
//     to keep metadata commands clean and user-friendly.
//  2. InitProvider() performs heavyweight initialization - discovers nodes,
//     network interfaces, and external container setup. This should only be called
//     before tests execute (e.g., in a BeforeAll hook) to avoid expensive operations
//     and verbose logging during metadata-only commands like 'list', 'info', or 'images'
type Provider interface {
	api.Provider
	LoadTestConfigs() error
	InitProvider() error
}

type openshift struct {
	engine            *container.Engine
	config            *rest.Config
	nodes             map[string]*ocpNode
	sshRunner         api.Runner
	HostPort          *portalloc.PortAllocator
	primaryNet        api.Network
	primaryNetGwIface *iface
}

type ocpNode struct {
	attachedIfaces map[string]*iface
}

type iface struct {
	ifName   string
	mac      string
	v4       string
	v4Subnet string
	v6       string
	v6Subnet string
}

func New(config *rest.Config) (Provider, error) {
	ovnkconfig.Kubernetes.DNSServiceNamespace = "openshift-dns"
	ovnkconfig.Kubernetes.DNSServiceName = "dns-default"
	// Initialize command runner for executing commands on hypervisor
	// (optional, may not be available)
	sshRunner, err := hypervisorSshCmdRunner()
	if err != nil {
		return nil, err
	}
	o := &openshift{HostPort: portalloc.New(30000, 32767),
		sshRunner: sshRunner,
		config:    config}
	if sshRunner != nil {
		// Initialize podman container engine
		o.engine = container.NewEngine("podman", sshRunner)
	}
	return o, err
}

func (o *openshift) LoadTestConfigs() error {
	// Fetch cluster configs once and reuse for all checks.
	// This optimization makes it easy to add more feature gate or network config checks
	// in the future without additional API calls.
	operatorClient, err := operatorv1client.NewForConfig(o.config)
	if err != nil {
		return fmt.Errorf("failed to retrieve operator client: %w", err)
	}

	configClient, err := configclient.NewForConfig(o.config)
	if err != nil {
		return fmt.Errorf("failed to retrieve config client: %w", err)
	}

	network, err := operatorClient.OperatorV1().Networks().Get(context.Background(), "cluster", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to retrieve network operator cluster object: %w", err)
	}

	clusterFeatureGate, err := configClient.ConfigV1().FeatureGates().Get(context.Background(), "cluster", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to retrieve cluster feature gate: %w", err)
	}

	// Configure test environment based on cluster configuration
	o.configureOVNGatewayMode(network)
	err = o.detectEVPNCapability(network, clusterFeatureGate)
	if err != nil {
		return fmt.Errorf("failed to check EVPN capability with the cluster: %w", err)
	}
	// Future feature detection can be added here, reusing network and clusterFeatureGate

	return nil
}

func (o *openshift) InitProvider() error {
	kubeClient, err := kubernetes.NewForConfig(o.config)
	if err != nil {
		return fmt.Errorf("unable to create kubernetes client: %w", err)
	}
	o.nodes, o.primaryNet, err = loadKubeNodes(kubeClient)
	if err != nil {
		return fmt.Errorf("unable to retrieve ocp nodes and primary network information: %w", err)
	}
	v4, v6, err := o.primaryNet.IPv4IPv6Subnets()
	if err != nil {
		return fmt.Errorf("failed to retrieve primary network subnets: %w", err)
	}
	if o.sshRunner == nil {
		return nil
	}
	// Retrieve primary network interface from hypervisor instance
	o.primaryNetGwIface, err = findHypervisorNodeInterface(o.sshRunner, v4, v6)
	return err
}

// configureOVNGatewayMode detects and configures the OVN gateway mode for tests
func (o *openshift) configureOVNGatewayMode(network *operv1.Network) {
	if network.Spec.DefaultNetwork.OVNKubernetesConfig == nil {
		return
	}

	if network.Spec.DefaultNetwork.OVNKubernetesConfig.GatewayConfig != nil &&
		network.Spec.DefaultNetwork.OVNKubernetesConfig.GatewayConfig.RoutingViaHost {
		os.Setenv(EnvVarOVNGatewayMode, "local")
	}
}

// detectEVPNCapability checks all EVPN prerequisites and enables EVPN tests if available
func (o *openshift) detectEVPNCapability(network *operv1.Network, featureGate *configv1.FeatureGate) error {
	if !hasEVPNFeatureGate(featureGate) {
		return nil
	}
	if !hasFRRRouteProvider(network) {
		return nil
	}
	if !isLocalGatewayMode(network) {
		return nil
	}
	exists, err := o.hasFRRExternalContainer()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	// All prerequisites met - enable EVPN tests
	os.Setenv(EnvVarEVPNFeatureEnabled, "true")
	return nil
}

// hasEVPNFeatureGate checks if the EVPN feature gate is enabled in the cluster
func hasEVPNFeatureGate(clusterFeatureGate *configv1.FeatureGate) bool {
	for _, featureGate := range clusterFeatureGate.Status.FeatureGates {
		for _, feature := range featureGate.Enabled {
			if feature.Name == "EVPN" {
				return true
			}
		}
	}
	return false
}

// hasFRRRouteProvider checks if FRR is configured as a routing capability provider.
func hasFRRRouteProvider(network *operv1.Network) bool {
	if network.Spec.AdditionalRoutingCapabilities == nil {
		return false
	}

	for _, raProvider := range network.Spec.AdditionalRoutingCapabilities.Providers {
		if raProvider == operv1.RoutingCapabilitiesProviderFRR {
			return true
		}
	}
	return false
}

// isLocalGatewayMode checks if OVN is configured with local gateway mode (routing via host).
func isLocalGatewayMode(network *operv1.Network) bool {
	if network.Spec.DefaultNetwork.OVNKubernetesConfig == nil {
		return false
	}

	return network.Spec.DefaultNetwork.OVNKubernetesConfig.GatewayConfig != nil &&
		network.Spec.DefaultNetwork.OVNKubernetesConfig.GatewayConfig.RoutingViaHost
}

// hasFRRExternalContainer checks if the FRR external container is available
func (o *openshift) hasFRRExternalContainer() (bool, error) {
	if o.engine == nil || o.sshRunner == nil {
		return false, nil
	}
	// Verify SSH connectivity works
	if _, err := o.sshRunner.Run("echo", "connection test"); err != nil {
		return false, fmt.Errorf("failed to check frr container status, connectivity check failed with hypervisor: %w", err)
	}
	state, err := o.engine.GetContainerState(externalFRRContainerName)
	if err != nil {
		return false, fmt.Errorf("failed to retrieve frr container state: %w", err)
	}
	return state != "", nil
}

func (o *openshift) GetExternalContainerNetworkInterface(container api.ExternalContainer, network api.Network) (api.NetworkInterface, error) {
	if container.Name == "frr" && network.Name() == primaryNetworkName {
		// frr container uses static ip configuration for ostestbm_net,
		// querying it with podman inspect returns empty values, so build
		// it explicitly.
		if o.sshRunner == nil {
			return api.NetworkInterface{}, fmt.Errorf("can not find gateway node for frr container")
		}
		if o.primaryNetGwIface == nil {
			return api.NetworkInterface{}, fmt.Errorf("can not find primary network gateway node for frr container")
		}
		return api.NetworkInterface{
			IPv4: frrContainerPrimaryNetIPv4, IPv6: frrContainerPrimaryNetIPv6,
			IPv4Gateway: o.primaryNetGwIface.v4, IPv6Gateway: o.primaryNetGwIface.v6,
			InfName:    "eth0",
			IPv4Prefix: o.primaryNetGwIface.v4Subnet, IPv6Prefix: o.primaryNetGwIface.v6Subnet}, nil
	}
	if o.engine == nil {
		return api.NetworkInterface{},
			fmt.Errorf("container engine not found, can not find network %s interface for the container %s", network.Name(), container.Name)
	}
	return o.engine.GetNetworkInterface(container.Name, network.Name())
}

func (o *openshift) ShutdownNode(nodeName string) error {
	return fmt.Errorf("ShutdownNode not implemented for OpenShift provider")
}

func (o *openshift) StartNode(nodeName string) error {
	return fmt.Errorf("StartNode not implemented for OpenShift provider")
}

func (o *openshift) GetDefaultTimeoutContext() *framework.TimeoutContext {
	timeouts := framework.NewTimeoutContext()
	timeouts.PodStart = 10 * time.Minute
	return timeouts
}

func (o openshift) PreloadImages(images []string) {
	// no-op: OpenShift clusters pull images at runtime
}

func (o *openshift) Name() string {
	return "openshift"
}

func (o *openshift) PrimaryNetwork() (api.Network, error) {
	return o.getNetwork(primaryNetworkName)
}

func (o *openshift) GetNetwork(name string) (api.Network, error) {
	// Override "kind" network queries with the actual primary network name
	if name == "kind" {
		framework.Logf("overriding kind network with actual primary network name %s for the query", primaryNetworkName)
		name = primaryNetworkName
	}
	return o.getNetwork(name)

}

func (o *openshift) getNetwork(name string) (api.Network, error) {
	// check primary network first.
	if name == primaryNetworkName {
		return o.primaryNet, nil
	}
	if o.sshRunner == nil {
		return nil, fmt.Errorf("network %s not found", name)
	}
	// fall back into container networks.
	return o.engine.GetNetwork(name)
}

func (o *openshift) GetK8HostPort() uint16 {
	return o.HostPort.Allocate()
}

func (o *openshift) GetK8NodeNetworkInterface(instance string, network api.Network) (api.NetworkInterface, error) {
	if node, ok := o.nodes[instance]; ok {
		if iface, ok := node.attachedIfaces[network.Name()]; ok {
			return api.NetworkInterface{InfName: iface.ifName, IPv4: iface.v4,
				IPv6: iface.v6, IPv4Prefix: iface.v4Subnet,
				IPv6Prefix: iface.v6Subnet}, nil
		}
	}
	return api.NetworkInterface{}, fmt.Errorf("network interface not found on instance %s for network %s", instance, network.Name())
}

func (o *openshift) ExecK8NodeCommand(nodeName string, cmd []string) (string, error) {
	if len(cmd) == 0 {
		return "", fmt.Errorf("insufficient command arguments")
	}
	cmd = append([]string{"debug", fmt.Sprintf("node/%s", nodeName), "--to-namespace=default",
		"--", "chroot", "/host"}, cmd...)
	ocDebugCmd := exec.Command("oc", cmd...)
	var stdout, stderr bytes.Buffer
	ocDebugCmd.Stdout = &stdout
	ocDebugCmd.Stderr = &stderr

	if err := ocDebugCmd.Run(); err != nil {
		return "", fmt.Errorf("failed to run command %q on node %s: %v, stdout: %s, stderr: %s", ocDebugCmd.String(), nodeName, err, stdout.String(), stderr.String())
	}
	return stdout.String(), nil
}

func (o *openshift) ExecExternalContainerCommand(container api.ExternalContainer, cmd []string) (string, error) {
	if o.engine == nil {
		return "", fmt.Errorf("container engine not found, can not execute command %v on the container %s", cmd, container.Name)
	}
	return o.engine.ExecExternalContainerCommand(container, cmd)
}

func (o *openshift) ExternalContainerPrimaryInterfaceName() string {
	if o.engine == nil {
		panic("container engine not found, can not retrieve external container primary interface")
	}
	return o.engine.ExternalContainerPrimaryInterfaceName()
}

func (o *openshift) GetExternalContainerLogs(container api.ExternalContainer) (string, error) {
	if o.engine == nil {
		return "", fmt.Errorf("container engine not found, can not retrieve logs from external container %s", container.Name)
	}
	return o.engine.GetExternalContainerLogs(container)
}

func (o *openshift) GetExternalContainerPort() uint16 {
	if o.engine == nil {
		panic("container engine not found, can not allocate port for external container")
	}
	return o.engine.GetExternalContainerPort()
}

func (o *openshift) ListNetworks() ([]string, error) {
	if o.engine == nil {
		return nil, fmt.Errorf("container engine not found, can not list networks")
	}
	return o.engine.ListNetworks()
}

func (o *openshift) NewTestContext() api.Context {
	context := &testcontext.TestContext{}
	ginkgo.DeferCleanup(context.CleanUp)
	co := &contextOpenshift{
		TestContext: context,
		engine:      o.engine.WithTestContext(context),
	}
	return co
}

type contextOpenshift struct {
	*testcontext.TestContext
	engine *container.Engine
}

func (o *contextOpenshift) CreateExternalContainer(container api.ExternalContainer) (api.ExternalContainer, error) {
	if o.engine == nil {
		return api.ExternalContainer{},
			fmt.Errorf("container engine not found, can not create external container %s", container.Name)
	}
	return o.engine.CreateExternalContainer(container)
}

func (o *contextOpenshift) DeleteExternalContainer(container api.ExternalContainer) error {
	if o.engine == nil {
		return fmt.Errorf("container engine not found, can not delete external container %s", container.Name)
	}
	return o.engine.DeleteExternalContainer(container)
}

func (o *contextOpenshift) CreateNetwork(name string, subnets ...string) (api.Network, error) {
	if o.engine == nil {
		return nil, fmt.Errorf("container engine not found, can not create network %s", name)
	}
	return o.engine.CreateNetwork(name, subnets...)
}

func (o *contextOpenshift) AttachNetwork(network api.Network, container string) (api.NetworkInterface, error) {
	if o.engine == nil {
		return api.NetworkInterface{},
			fmt.Errorf("container engine not found, can't attach network %s from container %s", network.Name(), container)
	}
	return o.engine.AttachNetwork(network, container)
}

func (o *contextOpenshift) DetachNetwork(network api.Network, container string) error {
	if o.engine == nil {
		return fmt.Errorf("container engine not found, can't detach network %s from container %s", network.Name(), container)
	}
	return o.engine.DetachNetwork(network, container)
}

func (o *contextOpenshift) DeleteNetwork(network api.Network) error {
	if o.engine == nil {
		return fmt.Errorf("container engine not found, can not delete network %s", network.Name())
	}
	return o.engine.DeleteNetwork(network)
}

func (c *contextOpenshift) SetupUnderlay(f *framework.Framework, underlay api.Underlay) error {
	return fmt.Errorf("SetupUnderlay is not supported")
}

func loadKubeNodes(kubeClient *kubernetes.Clientset) (map[string]*ocpNode, *network.ContainerEngineNetwork, error) {
	nodeMap := map[string]*ocpNode{}
	nodeList, err := kubeClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to retrieve nodes from the cluster: %w", err)
	}
	primaryNet := &network.ContainerEngineNetwork{NetName: primaryNetworkName}
	for _, node := range nodeList.Items {
		nodeIfAddrAnno, ok := node.Annotations[ovnAnnotationNodeIfAddr]
		if !ok {
			ginkgo.GinkgoLogr.Info("The annotation k8s.ovn.org/node-primary-ifaddr not found from node", "node", node.Name)
			continue
		}
		nodeIfAddr := make(map[string]string)
		if err := json.Unmarshal([]byte(nodeIfAddrAnno), &nodeIfAddr); err != nil {
			return nil, nil, fmt.Errorf("failed to parse node annotation %s: %w", ovnAnnotationNodeIfAddr, err)
		}
		nodeNetInfo := &iface{}
		kubeNode := &ocpNode{attachedIfaces: map[string]*iface{primaryNetworkName: nodeNetInfo}}
		var cidrs []network.ContainerEngineNetworkConfig
		if ip4, ok := nodeIfAddr["ipv4"]; ok {
			v4, cidr, err := net.ParseCIDR(ip4)
			if err != nil {
				return nil, nil, fmt.Errorf("unexpected error: node annotation ip %s entry is not a valid CIDR", ip4)
			}
			nodeNetInfo.v4 = v4.String()
			nodeNetInfo.v4Subnet = cidr.String()
			cidrs = append(cidrs, network.ContainerEngineNetworkConfig{Subnet: nodeNetInfo.v4Subnet})
		}
		if ip6, ok := nodeIfAddr["ipv6"]; ok {
			v6, cidr, err := net.ParseCIDR(ip6)
			if err != nil {
				return nil, nil, fmt.Errorf("unexpected error: node annotation ip %s entry is not a valid CIDR", ip6)
			}
			nodeNetInfo.v6 = v6.String()
			nodeNetInfo.v6Subnet = cidr.String()
			cidrs = append(cidrs, network.ContainerEngineNetworkConfig{Subnet: nodeNetInfo.v6Subnet})
		}
		if len(primaryNet.Configs) == 0 {
			// all nodes share same cidr, so assign first matching one.
			primaryNet.Configs = cidrs
		}
		ifName, err := findPrimaryInterface(kubeClient, node.Name)
		if err != nil {
			return nil, nil, err
		}
		nodeNetInfo.ifName = ifName
		nodeMap[node.Name] = kubeNode
	}
	return nodeMap, primaryNet, nil
}

func findPrimaryInterface(kubeClient *kubernetes.Clientset, nodeName string) (string, error) {
	ovnkubeNodePods, err := kubeClient.CoreV1().Pods(ovnpodNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return "", err
	}
	if len(ovnkubeNodePods.Items) != 1 {
		return "", fmt.Errorf("failed to find ovnkube-node pod for node instance %s", nodeName)
	}
	ovnKubeNodePodName := ovnkubeNodePods.Items[0].Name
	ports, err := e2epodoutput.RunHostCmd(ovnpodNamespace, ovnKubeNodePodName, "ovs-vsctl list-ports br-ex")
	if err != nil {
		return "", err
	}
	if ports == "" {
		return "", fmt.Errorf("no ports found on br-ex for node %s", nodeName)
	}
	for _, port := range strings.Split(ports, "\n") {
		if port == "" {
			continue
		}
		out, err := e2epodoutput.RunHostCmd(ovnpodNamespace, ovnKubeNodePodName, fmt.Sprintf("ovs-vsctl get Port %s Interfaces", port))
		if err != nil {
			return "", err
		}
		// remove brackets on list of interfaces
		ifaces := strings.Trim(strings.TrimSpace(out), "[]")
		for _, iface := range strings.Split(ifaces, ",") {
			out, err := e2epodoutput.RunHostCmd(ovnpodNamespace, ovnKubeNodePodName, fmt.Sprintf("ovs-vsctl get Interface %s Type", strings.TrimSpace(iface)))
			if err != nil {
				return "", err

			}
			// If system Type we know this is the OVS port is the NIC
			if strings.TrimSpace(out) == "system" {
				return port, nil
			}
		}
	}
	return "", fmt.Errorf("failed to find network interface from ovnkube-node pod %s", ovnKubeNodePodName)
}

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

	sshRunner, err := runner.NewSSHRunner(ip, hypervisorNodeUser, hypervisorSshport, sshKeyPath)
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

type linkInfo struct {
	IfName   string          `json:"ifname"`
	Mac      string          `json:"address"`
	AddrInfo []ipAddressInfo `json:"addr_info"`
}

type ipAddressInfo struct {
	Family string `json:"family"`
	Local  string `json:"local"`
}

// findHypervisorNodeInterface retrieves attached interface for the matching subnets from the hypervisor node.
func findHypervisorNodeInterface(runner api.Runner, v4Subnet, v6Subnet string) (*iface, error) {
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
