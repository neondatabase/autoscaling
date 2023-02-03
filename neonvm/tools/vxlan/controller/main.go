package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/bufbuild/connect-go"
	goipamapiv1 "github.com/cicdteam/go-ipam/api/v1"
	"github.com/cicdteam/go-ipam/api/v1/apiv1connect"

	"google.golang.org/grpc"

	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	VXLAN_IF_NAME     = "neon-vxlan0"
	VXLAN_BRIDGE_NAME = "neon-br0"
	VXLAN_ID          = 100

	ipamServerVariableName = "IPAM_SERVER"
)

var (
	delete = flag.Bool("delete", false, `delete VXLAN interfaces`)
)

func main() {
	flag.Parse()

	ipamService := os.Getenv(ipamServerVariableName)
	if len(ipamService) == 0 {
		log.Fatalf("IPAM service not found, environment variable %s is empty", ipamServerVariableName)
	}

	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	// -delete option used for teardown vxlan setup
	if *delete {
		log.Printf("deleting vxlan interface %s", VXLAN_IF_NAME)
		if err := deleteLink(VXLAN_IF_NAME); err != nil {
			log.Print(err)
		}
		log.Printf("deleting bridge interface %s", VXLAN_BRIDGE_NAME)
		if err := deleteLinkAndAddr(VXLAN_BRIDGE_NAME, ipamService); err != nil {
			log.Print(err)
		}
		os.Exit(0)
	}

	ownNodeIP := os.Getenv("MY_NODE_IP")
	log.Printf("own node IP: %s", ownNodeIP)

	// create linux bridge
	log.Printf("creating linux bridge interface (name: %s)", VXLAN_BRIDGE_NAME)
	if err := createBrigeInterface(VXLAN_BRIDGE_NAME, ipamService); err != nil {
		log.Fatal(err)
	}

	// create vxlan
	log.Printf("creating vxlan interface (name: %s, id: %d)", VXLAN_IF_NAME, VXLAN_ID)
	if err := createVxlanInterface(VXLAN_IF_NAME, VXLAN_ID, ownNodeIP, VXLAN_BRIDGE_NAME); err != nil {
		log.Fatal(err)
	}

	for {
		log.Print("getting nodes IP addresses")
		nodeIPs, err := getNodesIPs(clientset)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("found %d ip addresses", len(nodeIPs))

		// update FDB
		log.Print("udpate FDB table")
		if err := updateFDB(VXLAN_IF_NAME, nodeIPs, ownNodeIP); err != nil {
			log.Fatal(err)
		}

		time.Sleep(30 * time.Second)
	}
}

func getNodesIPs(clientset *kubernetes.Clientset) ([]string, error) {
	ips := []string{}
	nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return ips, err
	}
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				ips = append(ips, a.Address)
			}
		}
	}
	return ips, nil
}

func createBrigeInterface(name, ipam string) error {
	// check if interface already exists
	l, err := netlink.LinkByName(name)
	if err == nil {
		log.Printf("link with name %s already found", name)
		ips, _ := netlink.AddrList(l, netlink.FAMILY_V4)
		if len(ips) == 0 {
			log.Printf("no ip address found in %s", name)
			ip, mask, err := acquireIP(ipam)
			if err != nil {
				return err
			}
			log.Printf("setup IP %s on %s", ip.String(), name)
			linkAddr := &netlink.Addr{
				IPNet: &net.IPNet{
					IP:   ip,
					Mask: mask,
				},
			}
			if err := netlink.AddrReplace(l, linkAddr); err != nil {
				return err
			}
		}
		return nil
	}
	_, notFound := err.(netlink.LinkNotFoundError)
	if !notFound {
		return err
	}

	// create an configure linux bridge
	link := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: name,
		},
	}
	if err := netlink.LinkAdd(link); err != nil {
		return err
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}

	ip, mask, err := acquireIP(ipam)
	if err != nil {
		return err
	}
	log.Printf("setup IP %s on %s", ip.String(), name)
	linkAddr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ip,
			Mask: mask,
		},
	}
	if err := netlink.AddrReplace(link, linkAddr); err != nil {
		return err
	}

	return nil
}

func createVxlanInterface(name string, vxlanID int, ownIP string, bridgeName string) error {
	// check if interface already exists
	_, err := netlink.LinkByName(name)
	if err == nil {
		log.Printf("link with name %s already found", name)
		return nil
	}
	_, notFound := err.(netlink.LinkNotFoundError)
	if !notFound {
		return err
	}

	// create an configure vxlan
	link := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{
			Name: name,
		},
		VxlanId: vxlanID,
		SrcAddr: net.ParseIP(ownIP),
		Port:    4789,
	}

	if err := netlink.LinkAdd(link); err != nil {
		return err
	}

	// add vxlan to bridge
	br, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetMaster(link, br); err != nil {
		return err
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}

	return nil
}

func updateFDB(vxlanName string, nodeIPs []string, ownIP string) error {

	broadcastFdbMac, _ := net.ParseMAC("00:00:00:00:00:00")

	// get vxlan interface details
	link, err := netlink.LinkByName(vxlanName)
	if err != nil {
		return err
	}

	for _, ip := range nodeIPs {
		if ip != ownIP {
			broadcastFdbEntry := netlink.Neigh{
				LinkIndex:    link.Attrs().Index,
				Family:       syscall.AF_BRIDGE,
				State:        netlink.NUD_PERMANENT,
				Flags:        netlink.NTF_SELF,
				IP:           net.ParseIP(ip),
				HardwareAddr: broadcastFdbMac,
			}
			// add entry to FDB table
			// duplicate append action will not case error.
			log.Printf("add/update FDB broadcast entry via %s", ip)
			if err := netlink.NeighAppend(&broadcastFdbEntry); err != nil {
				return err
			}
		}
	}

	return nil
}

func deleteLink(name string) error {
	// check if interface already exists
	link, err := netlink.LinkByName(name)
	if err == nil {
		if err := netlink.LinkDel(link); err != nil {
			return err
		}
		log.Printf("link with name %s was deleted", name)
		return nil
	}
	_, notFound := err.(netlink.LinkNotFoundError)
	if !notFound {
		return err
	}
	log.Printf("link with name %s not found", name)

	return nil
}

func deleteLinkAndAddr(name, ipam string) error {
	// check if interface already exists
	link, lerr := netlink.LinkByName(name)
	if lerr == nil {
		ips, _ := netlink.AddrList(link, netlink.FAMILY_V4)
		for _, ip := range ips {
			log.Printf("releasing ip %s", ip.IP.String())
			if err := releaseIP(ipam, ip.IP); err != nil {
				log.Println(err)
			}
		}
		if err := netlink.LinkDel(link); err != nil {
			return err
		}
		log.Printf("link with name %s was deleted", name)
		return nil
	}
	_, notFound := lerr.(netlink.LinkNotFoundError)
	if !notFound {
		return lerr
	}
	log.Printf("link with name %s not found", name)

	return nil
}

func waitForGrpc(ctx context.Context, addr string) {
	dialAddr := strings.TrimLeft(addr, "http://")
	dialTimeout := 5 * time.Second
	dialOpts := []grpc.DialOption{
		grpc.WithInsecure(),
		grpc.WithBlock(),
	}
	log.Printf("check grpc connection to service %s", dialAddr)
	for {
		dialCtx, dialCancel := context.WithTimeout(ctx, dialTimeout)
		defer dialCancel()
		check, err := grpc.DialContext(dialCtx, dialAddr, dialOpts...)
		if err != nil {
			if err == context.DeadlineExceeded {
				log.Printf("timeout: failed to connect service %s", dialAddr)
			} else {
				log.Printf("failed to connect service at %s: %+v", dialAddr, err)
			}
		} else {
			log.Printf("connected, grpc connection state: %v", check.GetState())
			break
		}
	}
}

func acquireIP(ipam string) (net.IP, net.IPMask, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waitForGrpc(ctx, ipam)

	c := apiv1connect.NewIpamServiceClient(
		http.DefaultClient,
		ipam,
		connect.WithGRPC(),
	)

	// get prefixes from IPAM service
	prefixes, err := c.ListPrefixes(ctx, connect.NewRequest(&goipamapiv1.ListPrefixesRequest{}))
	if err != nil {
		return net.IP{}, net.IPMask{}, err
	}
	p := prefixes.Msg.Prefixes
	if len(p) == 0 {
		return net.IP{}, net.IPMask{}, fmt.Errorf("IPAM prefix not found")
	}
	if len(p) > 1 {
		return net.IP{}, net.IPMask{}, fmt.Errorf("too many IPAM prefixes found (%d)", len(p))
	}

	result, err := c.AcquireIP(ctx, connect.NewRequest(&goipamapiv1.AcquireIPRequest{PrefixCidr: p[0].Cidr}))
	if err != nil {
		return net.IP{}, net.IPMask{}, err
	}
	log.Printf("ip %s acquired", result.Msg.Ip.Ip)

	// parse overlay cidr for IPMask
	_, ipv4Net, err := net.ParseCIDR(p[0].Cidr)
	if err != nil {
		return net.IP{}, net.IPMask{}, err
	}
	ip := net.ParseIP(result.Msg.Ip.Ip)
	mask := ipv4Net.Mask

	return ip, mask, nil
}

func releaseIP(ipam string, ip net.IP) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waitForGrpc(ctx, ipam)

	c := apiv1connect.NewIpamServiceClient(
		http.DefaultClient,
		ipam,
		connect.WithGRPC(),
	)

	// get prefixes from IPAM service
	prefixes, err := c.ListPrefixes(ctx, connect.NewRequest(&goipamapiv1.ListPrefixesRequest{}))
	if err != nil {
		return err
	}
	p := prefixes.Msg.Prefixes
	if len(p) == 0 {
		return fmt.Errorf("IPAM prefix not found")
	}
	if len(p) > 1 {
		return fmt.Errorf("too many IPAM prefixes found (%d)", len(p))
	}

	result, err := c.ReleaseIP(ctx, connect.NewRequest(&goipamapiv1.ReleaseIPRequest{PrefixCidr: p[0].Cidr, Ip: ip.String()}))
	if err != nil {
		return err
	}
	log.Printf("ip %s released", result.Msg.Ip.Ip)

	return nil
}
