package controller

import (
	"fmt"
	"net"
	"reflect"
	"sync"

	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// The nodeController interface is implemented by the os-specific code
type nodeController interface {
	AddPod(*corev1.Pod) error
	DeletePod(*corev1.Pod) error
	AddNode(*corev1.Node) error
	DeleteNode(*corev1.Node) error
	RunFlowSync(<-chan struct{})
	EnsureHybridOverlayBridge(node *corev1.Node) error
}

// Node is a node controller and it's informers
type Node struct {
	ready            bool
	controller       nodeController
	nodeEventHandler informer.EventHandler
	podEventHandler  informer.EventHandler
	sync.Mutex
}

func (n *Node) IsReady() bool {
	n.Lock()
	defer n.Unlock()
	return n.ready
}

func (n *Node) setReady(b bool) {
	n.Lock()
	defer n.Unlock()
	n.ready = b
}

func nodeChanged(old, new interface{}) bool {
	oldNode := old.(*corev1.Node)
	newNode := new.(*corev1.Node)

	oldCidr, oldNodeIP, oldDrMAC, _ := getNodeDetails(oldNode)
	newCidr, newNodeIP, newDrMAC, _ := getNodeDetails(newNode)

	return !reflect.DeepEqual(oldCidr, newCidr) || !reflect.DeepEqual(oldNodeIP, newNodeIP) || !reflect.DeepEqual(oldDrMAC, newDrMAC) ||
		!reflect.DeepEqual(newNode.Annotations[hotypes.HybridOverlayDRIP], oldNode.Annotations[hotypes.HybridOverlayDRIP]) ||
		util.NoHostSubnet(oldNode) != util.NoHostSubnet(newNode)
}

// podChanged returns true if any relevant pod attributes changed
func podChanged(old, new interface{}) bool {
	oldPod := old.(*corev1.Pod)
	newPod := new.(*corev1.Pod)

	oldIPs, oldMAC, _ := getPodDetails(oldPod)
	newIPs, newMAC, _ := getPodDetails(newPod)

	if len(oldIPs) != len(newIPs) || !reflect.DeepEqual(oldMAC, newMAC) {
		return true
	}
	for i := range oldIPs {
		if oldIPs[i].String() != newIPs[i].String() {
			return true
		}
	}
	return false
}

// NewNode returns a new node controller
// This controller is designed to be used both by ovnkube-node binary and by the
// HO binary.
// When used by ovnkube-node binary, it prepares the OVN nodes for the HO tunnel.
// When used by the HO binary, it prepares the windows or SDN (SDN <-> OVN
// migration) nodes for the HO tunnel. This is flagged by setting isHONode to true.

// TODO(jtanenba) the localPodInformer no longer selects only local pods
func NewNode(
	kube kube.Interface,
	nodeName string,
	nodeInformer cache.SharedIndexInformer,
	localPodInformer cache.SharedIndexInformer,
	eventHandlerCreateFunction informer.EventHandlerCreateFunction,
	isHONode bool,
) (*Node, error) {

	nodeLister := listers.NewNodeLister(nodeInformer.GetIndexer())
	localPodLister := listers.NewPodLister(localPodInformer.GetIndexer())

	controller, err := newNodeController(kube, nodeName, nodeLister, localPodLister, isHONode)
	if err != nil {
		return nil, err
	}
	n := &Node{controller: controller}
	n.nodeEventHandler, err = eventHandlerCreateFunction("node", nodeInformer,
		func(obj interface{}) error {
			node, ok := obj.(*corev1.Node)
			if !ok {
				return fmt.Errorf("object is not a node")
			}
			return n.controller.AddNode(node)
		},
		func(obj interface{}) error {
			node, ok := obj.(*corev1.Node)
			if !ok {
				return fmt.Errorf("object is not a node")
			}
			return n.controller.DeleteNode(node)
		},
		nodeChanged,
	)
	if err != nil {
		return nil, err
	}
	n.podEventHandler, err = eventHandlerCreateFunction("pod", localPodInformer,
		func(obj interface{}) error {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return fmt.Errorf("object is not a pod")
			}
			if pod.Spec.NodeName != nodeName {
				return nil
			}
			return n.controller.AddPod(pod)
		},
		func(obj interface{}) error {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return fmt.Errorf("object is not a pod")
			}
			return n.controller.DeletePod(pod)
		},
		podChanged,
	)
	if err != nil {
		return nil, err
	}
	return n, nil

}

// Run starts the controller and does not return until all operations have
// terminated after the stop channel is closed
func (n *Node) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	klog.Info("Starting Hybrid Overlay Node Controller")

	wg := &sync.WaitGroup{}

	klog.Info("Starting Hybrid Overlay Node workers")
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := n.nodeEventHandler.Run(informer.DefaultNodeInformerThreadiness, stopCh)
		if err != nil {
			klog.Error(err)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := n.podEventHandler.Run(informer.DefaultInformerThreadiness, stopCh)
		if err != nil {
			klog.Error(err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		n.controller.RunFlowSync(stopCh)
	}()

	klog.Info("Started Hybrid Overlay Node workers")
	n.setReady(true)
	<-stopCh
	klog.Info("Shutting down Hybrid Overlay Node workers")
	wg.Wait()
	klog.Info("Shut down Hybrid Overlay Node workers")
}

// getNodeSubnetAndIP returns the node's hybrid overlay subnet and the node's
// first InternalIP, or nil if the subnet or node IP is invalid
func getNodeSubnetAndIP(node *corev1.Node) (*net.IPNet, net.IP) {
	var cidr *net.IPNet

	// Parse Linux node OVN hostsubnet annotation first
	cidrs, _ := util.ParseNodeHostSubnetAnnotation(node, ovntypes.DefaultNetworkName)
	if cidrs != nil {
		// FIXME DUAL-STACK
		cidr = cidrs[0]
	} else {
		// Otherwise parse the hybrid overlay node subnet annotation
		subnet, ok := node.Annotations[hotypes.HybridOverlayNodeSubnet]
		if !ok {

			klog.V(5).Infof("Missing node %q node subnet annotation", node.Name)
			return nil, nil
		}
		var err error
		_, cidr, err = net.ParseCIDR(subnet)
		if err != nil {
			klog.Errorf("Error parsing node %q subnet %q: %v", node.Name, subnet, err)
			return nil, nil
		}
	}

	nodeIP, err := houtil.GetNodeInternalIP(node)
	if err != nil {
		klog.Errorf("Error getting node %q internal IP: %v", node.Name, err)
		return nil, nil
	}

	return cidr, net.ParseIP(nodeIP)
}

// getNodeDetails returns the node's hybrid overlay subnet, first InternalIP,
// and the distributed router MAC (DRMAC), or nil if any of the addresses are
// missing or invalid.
func getNodeDetails(node *corev1.Node) (*net.IPNet, net.IP, net.HardwareAddr, error) {
	cidr, ip := getNodeSubnetAndIP(node)
	if cidr == nil || ip == nil {
		return nil, nil, nil, fmt.Errorf("missing node subnet and/or node IP")
	}

	drMACString, ok := node.Annotations[hotypes.HybridOverlayDRMAC]
	if !ok {
		return nil, nil, nil, fmt.Errorf("missing distributed router MAC annotation")
	}
	drMAC, err := net.ParseMAC(drMACString)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid distributed router MAC %q: %v", drMACString, err)
	}

	return cidr, ip, drMAC, nil
}

func getPodDetails(pod *corev1.Pod) ([]*net.IPNet, net.HardwareAddr, error) {
	podInfo, err := util.UnmarshalPodAnnotation(pod.Annotations, ovntypes.DefaultNetworkName)
	if err != nil {
		return nil, nil, err
	}
	return podInfo.IPs, podInfo.MAC, nil
}
