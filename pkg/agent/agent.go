package agent

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/vishvananda/netlink"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"okn/pkg/agent/openflow"
	"okn/pkg/iptables"
	"okn/pkg/ovs/ovsconfig"
)

const (
	TunPortName         = "tun0"
	tunOFPort           = 1
	hostGatewayOFPort   = 2
	maxRetryForHostLink = 5
	NodeNameKey         = "NODE_NAME"
)

type NodeConfig struct {
	Bridge  string
	Name    string
	PodCIDR *net.IPNet
	*Gateway
}

type Gateway struct {
	IP   net.IP
	MAC  net.HardwareAddr
	Name string
}

// Initializer knows how to setup host networking, OpenVSwitch, and Openflow.
type Initializer struct {
	ovsBridge       string
	hostGateway     string
	tunnelType      string
	client          clientset.Interface
	ifaceStore      InterfaceStore
	nodeConfig      *NodeConfig
	ovsBridgeClient ovsconfig.OVSBridgeClient
	serviceCIDR     *net.IPNet
	ofClient        openflow.Client
}

func disableICMPSendRedirects(intfName string) error {
	cmdStr := fmt.Sprintf("echo 0 > /proc/sys/net/ipv4/conf/%s/send_redirects", intfName)
	cmd := exec.Command("/bin/sh", "-c", cmdStr)
	if err := cmd.Run(); err != nil {
		klog.Errorf("Failed to disable send_redirect for interface %s: %v", intfName, err)
		return err
	}
	return nil
}

func NewInitializer(
	ovsBridgeClient ovsconfig.OVSBridgeClient,
	ofClient openflow.Client,
	k8sClient clientset.Interface,
	ovsBridge, serviceCIDR, hostGateway, tunnelType string,
	ifaceStore InterfaceStore) *Initializer {
	// Parse service CIDR configuration. serviceCIDR is checked in option.validate, so
	// it should be a valid configuration here.
	_, serviceCIDRNet, _ := net.ParseCIDR(serviceCIDR)
	return &Initializer{
		ovsBridgeClient: ovsBridgeClient,
		ovsBridge:       ovsBridge,
		hostGateway:     hostGateway,
		tunnelType:      tunnelType,
		client:          k8sClient,
		ifaceStore:      ifaceStore,
		serviceCIDR:     serviceCIDRNet,
		ofClient:        ofClient,
	}
}

// GetNodeConfig returns the NodeConfig.
func (i *Initializer) GetNodeConfig() *NodeConfig {
	return i.nodeConfig
}

// setupOVSBridge sets up the OVS bridge and create host gateway interface and tunnel port
func (i *Initializer) setupOVSBridge() error {
	if err := i.ovsBridgeClient.Create(); err != nil {
		klog.Error("Failed to create OVS bridge: ", err)
		return err
	}

	// Initialize interface cache
	if err := i.ifaceStore.Initialize(i.ovsBridgeClient, i.hostGateway, TunPortName); err != nil {
		return err
	}

	// Setup Tunnel port on OVS
	if err := i.setupTunnelInterface(TunPortName); err != nil {
		return err
	}
	// Setup host gateway interface
	err := i.setupGatewayInterface()
	if err != nil {
		return err
	}

	// send_redirects for the interface will be enabled if at least one of
	// conf/{all,interface}/send_redirects is set to TRUE, so "all" and the
	// interface must be disabled together.
	// See https://www.kernel.org/doc/Documentation/networking/ip-sysctl.txt.
	if err := disableICMPSendRedirects("all"); err != nil {
		return err
	}
	if err := disableICMPSendRedirects(i.hostGateway); err != nil {
		return err
	}
	return nil
}

func (i *Initializer) Initialize() error {
	klog.Info("Setting up node network")
	if err := i.initNodeLocalConfig(i.client); err != nil {
		return err
	}

	// Setup iptables chain and rules
	if err := iptables.SetupIPTables(); err != nil {
		return err
	}
	if err := iptables.SetupHostIPTablesRules(i.hostGateway); err != nil {
		return err
	}

	if err := i.setupOVSBridge(); err != nil {
		return err
	}

	// Install Openflow entries on OVS bridge
	if err := i.initOpenFlowPipeline(); err != nil {
		return err
	}

	return nil
}

// initOpenFlowPipeline sets up necessary Openflow entries, including pipeline, classifiers, conn_track, and gateway flows
func (i *Initializer) initOpenFlowPipeline() error {
	// Setup all basic flows.
	if err := i.ofClient.Initialize(); err != nil {
		klog.Errorf("Failed to setup basic openflow entries: %v", err)
		return err
	}

	// Setup flow entries for gateway interface, including classifier, skip spoof guard check,
	// L3 forwarding and L2 forwarding
	gateway, _ := i.ifaceStore.GetInterface(i.hostGateway)
	gatewayOFPort := uint32(gateway.OFPort)
	if err := i.ofClient.InstallGatewayFlows(gateway.IP, gateway.MAC, gatewayOFPort); err != nil {
		klog.Errorf("Failed to setup openflow entries for gateway: %v", err)
		return err
	}

	// Setup flow entries for tunnel port Interface, including classifier and L2 Forwarding(match
	// vMAC as dst)
	if err := i.ofClient.InstallTunnelFlows(tunOFPort); err != nil {
		klog.Errorf("Failed to setup openflow entries for tunnel interface: %v", err)
		return err
	}

	// Setup flow entries to enable service connectivity. Upstream kube-proxy is leveraged to
	// provide service feature, and this flow entry is to ensure traffic sent from pod to service
	// address could be forwarded to host gateway interface correctly. Otherwise packets might be
	// dropped by egress rules before they are DNATed to backend Pods.
	if err := i.ofClient.InstallServiceFlows(i.serviceCIDR.String(), i.serviceCIDR, gatewayOFPort); err != nil {
		klog.Errorf("Failed to setup openflow entries for serviceCIDR %s: %v", i.serviceCIDR, err)
		return err
	}
	return nil
}

// setupGatewayInterface creates the host gateway interface which is an internal port on OVS. The ofport for host
// gateway interface is predefined, so invoke CreateInternalPort with a specific ofport_request
func (i *Initializer) setupGatewayInterface() error {
	// Create host Gateway port if it does not exist
	gatewayIface, portExists := i.ifaceStore.GetInterface(i.hostGateway)
	if !portExists {
		klog.V(2).Infof("Creating gateway port %s on OVS bridge", i.hostGateway)
		gwPortUUID, err := i.ovsBridgeClient.CreateInternalPort(i.hostGateway, hostGatewayOFPort, nil)
		if err != nil {
			klog.Errorf("Failed to add host interface %s on OVS: %v", i.hostGateway, err)
			return err
		}
		gatewayIface = NewGatewayInterface(i.hostGateway)
		gatewayIface.OvsPortConfig = &OvsPortConfig{i.hostGateway, gwPortUUID, hostGatewayOFPort}
		i.ifaceStore.AddInterface(i.hostGateway, gatewayIface)
	} else {
		klog.V(2).Infof("Gateway port %s already exists on OVS bridge", i.hostGateway)
	}
	// host link might not be queried at once after create OVS internal port, retry max 5 times with 1s
	// delay each time to ensure the link is ready. If still failed after max retry return error.
	link, err := func() (netlink.Link, error) {
		for retry := 0; retry < maxRetryForHostLink; retry++ {
			if link, err := netlink.LinkByName(i.hostGateway); err != nil {
				klog.V(2).Infof("Not found host link for gateway %s, retry after 1s", i.hostGateway)
				if _, ok := err.(netlink.LinkNotFoundError); ok {
					time.Sleep(1 * time.Second)
				} else {
					return link, err
				}
			} else {
				return link, nil
			}
		}
		return nil, fmt.Errorf("link %s not found", i.hostGateway)
	}()
	if err != nil {
		klog.Errorf("Failed to find host link for gateway %s: %v", i.hostGateway, err)
		return err
	}

	// Set host gateway interface up
	if err := netlink.LinkSetUp(link); err != nil {
		klog.Errorf("Failed to set host link for %s up: %v", i.hostGateway, err)
		return err
	}

	// Configure host gateway IP using the first address of node localSubnet
	localSubnet := i.nodeConfig.PodCIDR
	subnetID := localSubnet.IP.Mask(localSubnet.Mask)
	gwIP := &net.IPNet{IP: ip.NextIP(subnetID), Mask: localSubnet.Mask}
	gwAddr := &netlink.Addr{IPNet: gwIP, Label: ""}
	gwMAC := link.Attrs().HardwareAddr
	i.nodeConfig.Gateway = &Gateway{Name: i.hostGateway, IP: gwIP.IP, MAC: gwMAC}
	gatewayIface.IP = gwIP.IP
	gatewayIface.MAC = gwMAC

	// Check IP address configuration on existing interface, return if already has target
	// address
	// We perform this check unconditionally, even if the OVS port did not exist when this
	// function was called (i.e. portExists is false). Indeed, it may be possible for the Linux
	// interface to exist even if the OVS bridge does not exist.
	if addrs, err := netlink.AddrList(link, netlink.FAMILY_V4); err != nil {
		klog.Errorf("Failed to query IPv4 address list for interface %s: %v", i.hostGateway, err)
		return err
	} else if addrs != nil {
		for _, addr := range addrs {
			klog.V(4).Infof("Found IPv4 address %s for interface %s", addr.IP.String(), i.hostGateway)
			if addr.IP.Equal(gwAddr.IPNet.IP) {
				klog.V(2).Infof("IPv4 address %s already assigned to interface %s", addr.IP.String(), i.hostGateway)
				return nil
			}
		}
	} else {
		klog.V(2).Infof("Link %s has no configured IPv4 address", i.hostGateway)
	}

	klog.V(2).Infof("Adding address %v to gateway interface %s", gwAddr, i.hostGateway)
	if err := netlink.AddrAdd(link, gwAddr); err != nil {
		klog.Errorf("Failed to set gateway interface %s with address %v: %v", i.hostGateway, gwAddr, err)
		return err
	}
	return nil
}

func (i *Initializer) setupTunnelInterface(tunnelPortName string) error {
	tunnelIface, portExists := i.ifaceStore.GetInterface(tunnelPortName)
	if portExists {
		klog.V(2).Infof("Tunnel port %s already exists on OVS", tunnelPortName)
		return nil
	}
	var err error
	var tunnelPortUUID string
	switch i.tunnelType {
	case ovsconfig.GENEVE_TUNNEL:
		tunnelPortUUID, err = i.ovsBridgeClient.CreateGenevePort(tunnelPortName, tunOFPort, "")
	case ovsconfig.VXLAN_TUNNEL:
		tunnelPortUUID, err = i.ovsBridgeClient.CreateVXLANPort(tunnelPortName, tunOFPort, "")
	default:
		err = fmt.Errorf("unsupported tunnel type %s", i.tunnelType)
	}
	if err != nil {
		klog.Errorf("Failed to add tunnel port %s type %s on OVS: %v", tunnelPortName, i.tunnelType, err)
		return err
	}
	tunnelIface = NewTunnelInterface(tunnelPortName)
	tunnelIface.OvsPortConfig = &OvsPortConfig{tunnelPortName, tunnelPortUUID, tunOFPort}
	i.ifaceStore.AddInterface(tunnelPortName, tunnelIface)
	return nil
}

// initNodeLocalConfig retrieves node's subnet CIDR from node.spec.PodCIDR, which is used for IPAM and setup
// host gateway interface.
func (i *Initializer) initNodeLocalConfig(client clientset.Interface) error {
	nodeName, err := getNodeName()
	if err != nil {
		return err
	}
	node, err := client.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
	if err != nil || node == nil {
		klog.Errorf("Failed to get node from K8s with name %s: %v", nodeName, err)
		return err
	}
	localCidr := node.Spec.PodCIDR
	_, localSubnet, err := net.ParseCIDR(localCidr)
	if err != nil {
		klog.Errorf("Failed to parse subnet from CIDR string %s: %v", localCidr, err)
		return err
	}

	i.nodeConfig = &NodeConfig{Name: nodeName, PodCIDR: localSubnet}
	return nil
}

// getNodeName returns the node's name used in Kubernetes, based on the priority:
// - Environment variable NODE_NAME, which should be set by Downward API
// - OS's hostname
func getNodeName() (string, error) {
	nodeName := os.Getenv(NodeNameKey)
	if nodeName != "" {
		return nodeName, nil
	}
	klog.Infof("Environment variable %s not found, use hostname instead", NodeNameKey)
	var err error
	nodeName, err = os.Hostname()
	if err != nil {
		klog.Errorf("Failed to get local hostname: %v", err)
		return "", err
	}
	return nodeName, nil
}