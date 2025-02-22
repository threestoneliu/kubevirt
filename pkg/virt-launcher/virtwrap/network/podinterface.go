/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2018 Red Hat, Inc.
 *
 */

//go:generate mockgen -source $GOFILE -package=$GOPACKAGE -destination=generated_mock_$GOFILE

package network

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strconv"
	"strings"

	netutils "k8s.io/utils/net"

	"kubevirt.io/kubevirt/pkg/util"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"

	v1 "kubevirt.io/client-go/api/v1"
	"kubevirt.io/client-go/log"
	"kubevirt.io/client-go/precond"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

var bridgeFakeIP = "169.254.75.1%d/32"

type BindMechanism interface {
	discoverPodNetworkInterface() error
	preparePodNetworkInterfaces(queueNumber uint32, launcherPID int) error

	loadCachedInterface(pid, name string) (bool, error)
	setCachedInterface(pid, name string) error

	// virt-handler that executes phase1 of network configuration needs to
	// pass details about discovered networking port into phase2 that is
	// executed by virt-launcher. Virt-launcher cannot discover some of
	// these details itself because at this point phase1 is complete and
	// ports are rewired, meaning, routes and IP addresses configured by
	// CNI plugin may be gone. For this matter, we use a cached VIF file to
	// pass discovered information between phases.
	loadCachedVIF(pid, name string) (bool, error)
	setCachedVIF(pid, name string) error

	// The following entry points require domain initialized for the
	// binding and can be used in phase2 only.
	decorateConfig() error
	startDHCP(vmi *v1.VirtualMachineInstance) error
}

type PodInterface struct{}

func (l *PodInterface) Unplug() {}

func getVifFilePath(pid, name string) string {
	return fmt.Sprintf(vifCacheFile, pid, name)
}

func writeVifFile(buf []byte, pid, name string) error {
	err := ioutil.WriteFile(getVifFilePath(pid, name), buf, 0644)
	if err != nil {
		return fmt.Errorf("error writing vif object: %v", err)
	}
	return nil
}

func setPodInterfaceCache(iface *v1.Interface, podInterfaceName string, uid string) error {
	cache := PodCacheInterface{Iface: iface}

	ipv4, ipv6, err := readIPAddressesFromLink(podInterfaceName)
	if err != nil {
		return err
	}

	switch {
	case ipv4 != "" && ipv6 != "":
		cache.PodIPs, err = sortIPsBasedOnPrimaryIP(ipv4, ipv6)
		if err != nil {
			return err
		}
	case ipv4 != "":
		cache.PodIPs = []string{ipv4}
	case ipv6 != "":
		cache.PodIPs = []string{ipv6}
	default:
		return nil
	}

	cache.PodIP = cache.PodIPs[0]
	err = writeToCachedFile(cache, util.VMIInterfacepath, uid, iface.Name)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to write pod Interface to cache, %s", err.Error())
		return err
	}

	return nil
}

func readIPAddressesFromLink(podInterfaceName string) (string, string, error) {
	link, err := Handler.LinkByName(podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get a link for interface: %s", podInterfaceName)
		return "", "", err
	}

	// get IP address
	addrList, err := Handler.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get a address for interface: %s", podInterfaceName)
		return "", "", err
	}

	// no ip assigned. ipam disabled
	if len(addrList) == 0 {
		return "", "", nil
	}

	var ipv4, ipv6 string
	for _, addr := range addrList {
		if addr.IP.IsGlobalUnicast() {
			if netutils.IsIPv6(addr.IP) && ipv6 == "" {
				ipv6 = addr.IP.String()
			} else if !netutils.IsIPv6(addr.IP) && ipv4 == "" {
				ipv4 = addr.IP.String()
			}
		}
	}

	return ipv4, ipv6, nil
}

// sortIPsBasedOnPrimaryIP returns a sorted slice of IP/s based on the detected cluster primary IP.
// The operation clones the Pod status IP list order logic.
func sortIPsBasedOnPrimaryIP(ipv4, ipv6 string) ([]string, error) {
	ipv4Primary, err := Handler.IsIpv4Primary()
	if err != nil {
		return nil, err
	}

	if ipv4Primary {
		return []string{ipv4, ipv6}, nil
	}

	return []string{ipv6, ipv4}, nil
}

func (l *PodInterface) PlugPhase1(vmi *v1.VirtualMachineInstance, iface *v1.Interface, network *v1.Network, podInterfaceName string, pid int) error {
	initHandler()

	// There is nothing to plug for SR-IOV devices
	if iface.SRIOV != nil {
		return nil
	}

	driver, err := getPhase1Binding(vmi, iface, network, podInterfaceName)
	if err != nil {
		return err
	}

	pidStr := fmt.Sprintf("%d", pid)
	isExist, err := driver.loadCachedInterface(pidStr, iface.Name)
	if err != nil {
		return err
	}

	// ignore the driver.loadCachedInterface for slirp and set the Pod interface cache
	if !isExist || iface.Slirp != nil {
		err := setPodInterfaceCache(iface, podInterfaceName, string(vmi.ObjectMeta.UID))
		if err != nil {
			return err
		}
	}
	if !isExist {
		err = driver.discoverPodNetworkInterface()
		if err != nil {
			return err
		}

		queueNumber := uint32(0)
		isMultiqueue := (vmi.Spec.Domain.Devices.NetworkInterfaceMultiQueue != nil) && (*vmi.Spec.Domain.Devices.NetworkInterfaceMultiQueue)
		if isMultiqueue {
			queueNumber = api.CalculateNetworkQueues(vmi)
		}
		if err := driver.preparePodNetworkInterfaces(queueNumber, pid); err != nil {
			log.Log.Reason(err).Error("failed to prepare pod networking")
			return createCriticalNetworkError(err)
		}

		err = driver.setCachedInterface(pidStr, iface.Name)
		if err != nil {
			log.Log.Reason(err).Error("failed to save interface configuration")
			return createCriticalNetworkError(err)
		}

		err = driver.setCachedVIF(pidStr, iface.Name)
		if err != nil {
			log.Log.Reason(err).Error("failed to save vif configuration")
			return createCriticalNetworkError(err)
		}
	}

	return nil
}

func createCriticalNetworkError(err error) *CriticalNetworkError {
	return &CriticalNetworkError{fmt.Sprintf("Critical network error: %v", err)}
}

func ensureDHCP(vmi *v1.VirtualMachineInstance, driver BindMechanism, podInterfaceName string) error {
	dhcpStartedFile := fmt.Sprintf("/var/run/kubevirt-private/dhcp_started-%s", podInterfaceName)
	_, err := os.Stat(dhcpStartedFile)
	if os.IsNotExist(err) {
		if err := driver.startDHCP(vmi); err != nil {
			return fmt.Errorf("failed to start DHCP server for interface %s", podInterfaceName)
		}
		newFile, err := os.Create(dhcpStartedFile)
		if err != nil {
			return fmt.Errorf("failed to create dhcp started file %s: %s", dhcpStartedFile, err)
		}
		newFile.Close()
	}
	return nil
}

func (l *PodInterface) PlugPhase2(vmi *v1.VirtualMachineInstance, iface *v1.Interface, network *v1.Network, domain *api.Domain, podInterfaceName string) error {
	precond.MustNotBeNil(domain)
	initHandler()

	// There is nothing to plug for SR-IOV devices
	if iface.SRIOV != nil {
		return nil
	}

	driver, err := getPhase2Binding(vmi, iface, network, domain, podInterfaceName)
	if err != nil {
		return err
	}

	pid := "self"

	isExist, err := driver.loadCachedInterface(pid, iface.Name)
	if err != nil {
		log.Log.Reason(err).Critical("failed to load cached interface configuration")
	}
	if !isExist {
		log.Log.Reason(err).Critical("cached interface configuration doesn't exist")
	}

	isExist, err = driver.loadCachedVIF(pid, iface.Name)
	if err != nil {
		log.Log.Reason(err).Critical("failed to load cached vif configuration")
	}
	if !isExist {
		log.Log.Reason(err).Critical("cached vif configuration doesn't exist")
	}

	err = driver.decorateConfig()
	if err != nil {
		log.Log.Reason(err).Critical("failed to create libvirt configuration")
	}

	err = ensureDHCP(vmi, driver, podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Criticalf("failed to ensure dhcp service running for %s: %s", podInterfaceName, err)
		panic(err)
	}

	return nil
}

// The only difference between bindings for two phases is that the first phase
// should not require access to domain definition, hence we pass nil instead of
// it. This means that any functions called under phase1 code path should not
// use the domain set on the binding.
func getPhase1Binding(vmi *v1.VirtualMachineInstance, iface *v1.Interface, network *v1.Network, podInterfaceName string) (BindMechanism, error) {
	return getPhase2Binding(vmi, iface, network, nil, podInterfaceName)
}

func getPhase2Binding(vmi *v1.VirtualMachineInstance, iface *v1.Interface, network *v1.Network, domain *api.Domain, podInterfaceName string) (BindMechanism, error) {
	populateMacAddress := func(vif *VIF, iface *v1.Interface) error {
		if iface.MacAddress != "" {
			macAddress, err := net.ParseMAC(iface.MacAddress)
			if err != nil {
				return err
			}
			vif.MAC = macAddress
		}
		return nil
	}

	if iface.Bridge != nil {
		vif := &VIF{Name: podInterfaceName}
		populateMacAddress(vif, iface)
		return &BridgePodInterface{iface: iface,
			virtIface:           &api.Interface{},
			vmi:                 vmi,
			vif:                 vif,
			domain:              domain,
			podInterfaceName:    podInterfaceName,
			bridgeInterfaceName: fmt.Sprintf("k6t-%s", podInterfaceName)}, nil
	}
	if iface.Masquerade != nil {
		vif := &VIF{Name: podInterfaceName}
		populateMacAddress(vif, iface)
		return &MasqueradePodInterface{iface: iface,
			virtIface:           &api.Interface{},
			vmi:                 vmi,
			vif:                 vif,
			domain:              domain,
			podInterfaceName:    podInterfaceName,
			vmNetworkCIDR:       network.Pod.VMNetworkCIDR,
			vmIpv6NetworkCIDR:   "", // TODO add ipv6 cidr to PodNetwork schema
			bridgeInterfaceName: fmt.Sprintf("k6t-%s", podInterfaceName)}, nil
	}
	if iface.Slirp != nil {
		return &SlirpPodInterface{vmi: vmi, iface: iface, domain: domain}, nil
	}
	if iface.Macvtap != nil {
		vif := &VIF{Name: podInterfaceName}
		populateMacAddress(vif, iface)
		return &MacvtapPodInterface{
			vmi:              vmi,
			vif:              vif,
			iface:            iface,
			virtIface:        &api.Interface{},
			domain:           domain,
			podInterfaceName: podInterfaceName,
		}, nil
	}
	return nil, fmt.Errorf("Not implemented")
}

type BridgePodInterface struct {
	vmi                 *v1.VirtualMachineInstance
	vif                 *VIF
	iface               *v1.Interface
	virtIface           *api.Interface
	podNicLink          netlink.Link
	domain              *api.Domain
	podInterfaceName    string
	bridgeInterfaceName string
}

func (b *BridgePodInterface) discoverPodNetworkInterface() error {
	link, err := Handler.LinkByName(b.podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get a link for interface: %s", b.podInterfaceName)
		return err
	}
	b.podNicLink = link

	// get IP address
	addrList, err := Handler.AddrList(b.podNicLink, netlink.FAMILY_V4)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get an ip address for %s", b.podInterfaceName)
		return err
	}
	if len(addrList) == 0 {
		b.vif.IPAMDisabled = true
	} else {
		b.vif.IP = addrList[0]
		b.vif.IPAMDisabled = false
	}

	if len(b.vif.MAC) == 0 {
		// Get interface MAC address
		mac, err := Handler.GetMacDetails(b.podInterfaceName)
		if err != nil {
			log.Log.Reason(err).Errorf("failed to get MAC for %s", b.podInterfaceName)
			return err
		}
		b.vif.MAC = mac
	}

	if b.podNicLink.Attrs().MTU < 0 || b.podNicLink.Attrs().MTU > 65535 {
		return fmt.Errorf("MTU value out of range ")
	}

	// Get interface MTU
	b.vif.Mtu = uint16(b.podNicLink.Attrs().MTU)

	if !b.vif.IPAMDisabled {
		// Handle interface routes
		if err := b.setInterfaceRoutes(); err != nil {
			return err
		}
	}
	return nil
}

func (b *BridgePodInterface) getFakeBridgeIP() (string, error) {
	ifaces := b.vmi.Spec.Domain.Devices.Interfaces
	for i, iface := range ifaces {
		if iface.Name == b.iface.Name {
			return fmt.Sprintf(bridgeFakeIP, i), nil
		}
	}
	return "", fmt.Errorf("Failed to generate bridge fake address for interface %s", b.iface.Name)
}

func (b *BridgePodInterface) startDHCP(vmi *v1.VirtualMachineInstance) error {
	if !b.vif.IPAMDisabled {
		addr, err := b.getFakeBridgeIP()
		if err != nil {
			return err
		}
		fakeServerAddr, err := netlink.ParseAddr(addr)
		if err != nil {
			return fmt.Errorf("failed to parse address while starting DHCP server: %s", addr)
		}
		log.Log.Object(b.vmi).Infof("bridge pod interface: %+v %+v", b.vif, b)
		return Handler.StartDHCP(b.vif, fakeServerAddr.IP, b.bridgeInterfaceName, b.iface.DHCPOptions)
	}
	return nil
}

func (b *BridgePodInterface) preparePodNetworkInterfaces(queueNumber uint32, launcherPID int) error {
	// Set interface link to down to change its MAC address
	if err := Handler.LinkSetDown(b.podNicLink); err != nil {
		log.Log.Reason(err).Errorf("failed to bring link down for interface: %s", b.podInterfaceName)
		return err
	}

	if _, err := Handler.SetRandomMac(b.podInterfaceName); err != nil {
		return err
	}

	if err := Handler.LinkSetUp(b.podNicLink); err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", b.podInterfaceName)
		return err
	}

	if err := b.createBridge(); err != nil {
		return err
	}

	tapDeviceName := generateTapDeviceName(podInterfaceName)
	err := createAndBindTapToBridge(b.vif, tapDeviceName, b.bridgeInterfaceName, queueNumber, launcherPID, int(b.vif.Mtu))
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create tap device named %s", tapDeviceName)
		return err
	}

	if !b.vif.IPAMDisabled {
		// Remove IP from POD interface
		err := Handler.AddrDel(b.podNicLink, &b.vif.IP)

		if err != nil {
			log.Log.Reason(err).Errorf("failed to delete address for interface: %s", b.podInterfaceName)
			return err
		}
	}

	if err := Handler.LinkSetLearningOff(b.podNicLink); err != nil {
		log.Log.Reason(err).Errorf("failed to disable mac learning for interface: %s", b.podInterfaceName)
		return err
	}

	b.virtIface.MTU = &api.MTU{Size: strconv.Itoa(b.podNicLink.Attrs().MTU)}
	b.virtIface.MAC = &api.MAC{MAC: b.vif.MAC.String()}
	b.virtIface.Target = &api.InterfaceTarget{
		Device:  b.vif.TapDevice,
		Managed: "no",
	}

	return nil
}

func (b *BridgePodInterface) decorateConfig() error {
	ifaces := b.domain.Spec.Devices.Interfaces
	for i, iface := range ifaces {
		if iface.Alias.Name == b.iface.Name {
			ifaces[i].MTU = b.virtIface.MTU
			ifaces[i].MAC = &api.MAC{MAC: b.vif.MAC.String()}
			ifaces[i].Target = b.virtIface.Target
			break
		}
	}
	return nil
}

func (b *BridgePodInterface) loadCachedInterface(pid, name string) (bool, error) {
	var ifaceConfig api.Interface

	isExist, err := readFromCachedFile(pid, name, interfaceCacheFile, &ifaceConfig)
	if err != nil {
		return false, err
	}

	if isExist {
		b.virtIface = &ifaceConfig
		return true, nil
	}

	return false, nil
}

func (b *BridgePodInterface) setCachedInterface(pid, name string) error {
	err := writeToCachedFile(b.virtIface, interfaceCacheFile, pid, name)
	return err
}

func (b *BridgePodInterface) loadCachedVIF(pid, name string) (bool, error) {
	buf, err := ioutil.ReadFile(getVifFilePath(pid, name))
	if err != nil {
		return false, err
	}
	err = json.Unmarshal(buf, &b.vif)
	if err != nil {
		return false, err
	}
	b.vif.Gateway = b.vif.Gateway.To4()
	return true, nil
}

func (b *BridgePodInterface) setCachedVIF(pid, name string) error {
	buf, err := json.MarshalIndent(&b.vif, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshaling vif object: %v", err)
	}
	return writeVifFile(buf, pid, name)
}

func (b *BridgePodInterface) setInterfaceRoutes() error {
	routes, err := Handler.RouteList(b.podNicLink, netlink.FAMILY_V4)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get routes for %s", b.podInterfaceName)
		return err
	}
	if len(routes) == 0 {
		return fmt.Errorf("No gateway address found in routes for %s", b.podInterfaceName)
	}
	b.vif.Gateway = routes[0].Gw
	if len(routes) > 1 {
		dhcpRoutes := filterPodNetworkRoutes(routes, b.vif)
		b.vif.Routes = &dhcpRoutes
	}
	return nil
}

func (b *BridgePodInterface) createBridge() error {
	// Create a bridge
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: b.bridgeInterfaceName,
		},
	}
	err := Handler.LinkAdd(bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create a bridge")
		return err
	}

	err = Handler.LinkSetMaster(b.podNicLink, bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to connect interface %s to bridge %s", b.podInterfaceName, bridge.Name)
		return err
	}

	err = Handler.LinkSetUp(bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", b.bridgeInterfaceName)
		return err
	}

	// set fake ip on a bridge
	addr, err := b.getFakeBridgeIP()
	if err != nil {
		return err
	}
	fakeaddr, err := Handler.ParseAddr(addr)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", b.bridgeInterfaceName)
		return err
	}

	if err := Handler.AddrAdd(bridge, fakeaddr); err != nil {
		log.Log.Reason(err).Errorf("failed to set bridge IP")
		return err
	}

	if err = Handler.DisableTXOffloadChecksum(b.bridgeInterfaceName); err != nil {
		log.Log.Reason(err).Error("failed to disable TX offload checksum on bridge interface")
		return err
	}

	return nil
}

type MasqueradePodInterface struct {
	vmi                 *v1.VirtualMachineInstance
	vif                 *VIF
	iface               *v1.Interface
	virtIface           *api.Interface
	podNicLink          netlink.Link
	domain              *api.Domain
	podInterfaceName    string
	bridgeInterfaceName string
	vmNetworkCIDR       string
	vmIpv6NetworkCIDR   string
	gatewayAddr         *netlink.Addr
	gatewayIpv6Addr     *netlink.Addr
}

func (p *MasqueradePodInterface) discoverPodNetworkInterface() error {
	link, err := Handler.LinkByName(p.podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get a link for interface: %s", p.podInterfaceName)
		return err
	}
	p.podNicLink = link

	if p.podNicLink.Attrs().MTU < 0 || p.podNicLink.Attrs().MTU > 65535 {
		return fmt.Errorf("MTU value out of range ")
	}

	// Get interface MTU
	p.vif.Mtu = uint16(p.podNicLink.Attrs().MTU)

	err = configureVifV4Addresses(p, err)
	if err != nil {
		return err
	}

	ipv6Enabled, err := Handler.IsIpv6Enabled(p.podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to verify whether ipv6 is configured on %s", p.podInterfaceName)
		return err
	}
	if ipv6Enabled {
		err = configureVifV6Addresses(p, err)
		if err != nil {
			return err
		}
	}
	return nil
}

func configureVifV4Addresses(p *MasqueradePodInterface, err error) error {
	if p.vmNetworkCIDR == "" {
		p.vmNetworkCIDR = api.DefaultVMCIDR
	}

	defaultGateway, vm, err := Handler.GetHostAndGwAddressesFromCIDR(p.vmNetworkCIDR)
	if err != nil {
		log.Log.Errorf("failed to get gw and vm available addresses from CIDR %s", p.vmNetworkCIDR)
		return err
	}

	gatewayAddr, err := Handler.ParseAddr(defaultGateway)
	if err != nil {
		return fmt.Errorf("failed to parse gateway ip address %s", defaultGateway)
	}
	p.vif.Gateway = gatewayAddr.IP.To4()
	p.gatewayAddr = gatewayAddr

	vmAddr, err := Handler.ParseAddr(vm)
	if err != nil {
		return fmt.Errorf("failed to parse vm ip address %s", vm)
	}
	p.vif.IP = *vmAddr
	return nil
}

func configureVifV6Addresses(p *MasqueradePodInterface, err error) error {
	if p.vmIpv6NetworkCIDR == "" {
		p.vmIpv6NetworkCIDR = api.DefaultVMIpv6CIDR
	}

	defaultGatewayIpv6, vmIpv6, err := Handler.GetHostAndGwAddressesFromCIDR(p.vmIpv6NetworkCIDR)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get gw and vm available ipv6 addresses from CIDR %s", p.vmIpv6NetworkCIDR)
		return err
	}

	gatewayIpv6Addr, err := Handler.ParseAddr(defaultGatewayIpv6)
	if err != nil {
		return fmt.Errorf("failed to parse gateway ipv6 address %s err %v", gatewayIpv6Addr, err)
	}
	p.vif.GatewayIpv6 = gatewayIpv6Addr.IP.To16()
	p.gatewayIpv6Addr = gatewayIpv6Addr

	vmAddr, err := Handler.ParseAddr(vmIpv6)
	if err != nil {
		return fmt.Errorf("failed to parse vm ipv6 address %s err %v", vmIpv6, err)
	}
	p.vif.IPv6 = *vmAddr
	return nil
}

func (p *MasqueradePodInterface) startDHCP(vmi *v1.VirtualMachineInstance) error {
	return Handler.StartDHCP(p.vif, p.vif.Gateway, p.bridgeInterfaceName, p.iface.DHCPOptions)
}

func (p *MasqueradePodInterface) preparePodNetworkInterfaces(queueNumber uint32, launcherPID int) error {
	// Create an master bridge interface
	bridgeNicName := fmt.Sprintf("%s-nic", p.bridgeInterfaceName)
	bridgeNic := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{
			Name: bridgeNicName,
			MTU:  int(p.vif.Mtu),
		},
	}
	err := Handler.LinkAdd(bridgeNic)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create an interface: %s", bridgeNic.Name)
		return err
	}

	if p.iface.MacAddress == "" {
		p.vif.MAC, err = Handler.GenerateRandomMac()
		if err != nil {
			log.Log.Reason(err).Errorf("failed to generate random mac address")
			return err
		}
	}

	err = Handler.LinkSetUp(bridgeNic)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", bridgeNic.Name)
		return err
	}

	if err := p.createBridge(); err != nil {
		return err
	}

	tapDeviceName := generateTapDeviceName(podInterfaceName)
	err = createAndBindTapToBridge(p.vif, tapDeviceName, p.bridgeInterfaceName, queueNumber, launcherPID, int(p.vif.Mtu))
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create tap device named %s", tapDeviceName)
		return err
	}

	if Handler.HasNatIptables(iptables.ProtocolIPv4) || Handler.NftablesLoad("ipv4-nat") == nil {
		err = p.createNatRules(iptables.ProtocolIPv4)
		if err != nil {
			log.Log.Reason(err).Errorf("failed to create ipv4 nat rules for vm error: %v", err)
			return err
		}
	} else {
		return fmt.Errorf("Couldn't configure ipv4 nat rules")
	}

	ipv6Enabled, err := Handler.IsIpv6Enabled(p.podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to verify whether ipv6 is configured on %s", p.podInterfaceName)
		return err
	}
	if ipv6Enabled {
		if Handler.HasNatIptables(iptables.ProtocolIPv6) || Handler.NftablesLoad("ipv6-nat") == nil {
			err = Handler.ConfigureIpv6Forwarding()
			if err != nil {
				log.Log.Reason(err).Errorf("failed to configure ipv6 forwarding")
				return err
			}

			err = p.createNatRules(iptables.ProtocolIPv6)
			if err != nil {
				log.Log.Reason(err).Errorf("failed to create ipv6 nat rules for vm error: %v", err)
				return err
			}
		} else {
			return fmt.Errorf("Couldn't configure ipv6 nat rules")
		}
	}

	p.virtIface.MTU = &api.MTU{Size: strconv.Itoa(p.podNicLink.Attrs().MTU)}
	p.virtIface.MAC = &api.MAC{MAC: p.vif.MAC.String()}
	p.virtIface.Target = &api.InterfaceTarget{
		Device:  p.vif.TapDevice,
		Managed: "no",
	}

	return nil
}

func (p *MasqueradePodInterface) decorateConfig() error {
	ifaces := p.domain.Spec.Devices.Interfaces
	for i, iface := range ifaces {
		if iface.Alias.Name == p.iface.Name {
			ifaces[i].MTU = p.virtIface.MTU
			ifaces[i].MAC = &api.MAC{MAC: p.vif.MAC.String()}
			ifaces[i].Target = p.virtIface.Target
			break
		}
	}
	return nil
}

func (p *MasqueradePodInterface) loadCachedInterface(pid, name string) (bool, error) {
	var ifaceConfig api.Interface

	isExist, err := readFromCachedFile(pid, name, interfaceCacheFile, &ifaceConfig)
	if err != nil {
		return false, err
	}

	if isExist {
		p.virtIface = &ifaceConfig
		return true, nil
	}

	return false, nil
}

func (p *MasqueradePodInterface) setCachedInterface(pid, name string) error {
	err := writeToCachedFile(p.virtIface, interfaceCacheFile, pid, name)
	return err
}

func (p *MasqueradePodInterface) loadCachedVIF(pid, name string) (bool, error) {
	buf, err := ioutil.ReadFile(getVifFilePath(pid, name))
	if err != nil {
		return false, err
	}
	err = json.Unmarshal(buf, &p.vif)
	if err != nil {
		return false, err
	}
	p.vif.Gateway = p.vif.Gateway.To4()
	p.vif.GatewayIpv6 = p.vif.GatewayIpv6.To16()
	return true, nil
}

func (p *MasqueradePodInterface) setCachedVIF(pid, name string) error {
	buf, err := json.MarshalIndent(&p.vif, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshaling vif object: %v", err)
	}
	return writeVifFile(buf, pid, name)
}

func (p *MasqueradePodInterface) createBridge() error {
	// Get dummy link
	bridgeNicName := fmt.Sprintf("%s-nic", p.bridgeInterfaceName)
	bridgeNicLink, err := Handler.LinkByName(bridgeNicName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to find dummy interface for bridge")
		return err
	}

	// Create a bridge
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: p.bridgeInterfaceName,
			MTU:  int(p.vif.Mtu),
		},
	}
	err = Handler.LinkAdd(bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create a bridge")
		return err
	}

	err = Handler.LinkSetMaster(bridgeNicLink, bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to connect %s interface to bridge %s", bridgeNicName, p.bridgeInterfaceName)
		return err
	}

	err = Handler.LinkSetUp(bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", p.bridgeInterfaceName)
		return err
	}

	if err := Handler.AddrAdd(bridge, p.gatewayAddr); err != nil {
		log.Log.Reason(err).Errorf("failed to set bridge IP")
		return err
	}

	ipv6Enabled, err := Handler.IsIpv6Enabled(p.podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to verify whether ipv6 is configured on %s", p.podInterfaceName)
		return err
	}
	if ipv6Enabled {
		if err := Handler.AddrAdd(bridge, p.gatewayIpv6Addr); err != nil {
			log.Log.Reason(err).Errorf("failed to set bridge IPv6")
			return err
		}
	}

	if err = Handler.DisableTXOffloadChecksum(p.bridgeInterfaceName); err != nil {
		log.Log.Reason(err).Error("failed to disable TX offload checksum on bridge interface")
		return err
	}

	return nil
}

func (p *MasqueradePodInterface) createNatRules(protocol iptables.Protocol) error {
	if Handler.HasNatIptables(protocol) {
		return p.createNatRulesUsingIptables(protocol)
	}
	return p.createNatRulesUsingNftables(protocol)
}

func (p *MasqueradePodInterface) createNatRulesUsingIptables(protocol iptables.Protocol) error {
	err := Handler.IptablesNewChain(protocol, "nat", "KUBEVIRT_PREINBOUND")
	if err != nil {
		return err
	}

	err = Handler.IptablesNewChain(protocol, "nat", "KUBEVIRT_POSTINBOUND")
	if err != nil {
		return err
	}

	err = Handler.IptablesAppendRule(protocol, "nat", "POSTROUTING", "-s", p.getVifIpByProtocol(protocol), "-j", "MASQUERADE")
	if err != nil {
		return err
	}

	err = Handler.IptablesAppendRule(protocol, "nat", "PREROUTING", "-i", p.podInterfaceName, "-j", "KUBEVIRT_PREINBOUND")
	if err != nil {
		return err
	}

	err = Handler.IptablesAppendRule(protocol, "nat", "POSTROUTING", "-o", p.bridgeInterfaceName, "-j", "KUBEVIRT_POSTINBOUND")
	if err != nil {
		return err
	}

	if len(p.iface.Ports) == 0 {
		err = Handler.IptablesAppendRule(protocol, "nat", "KUBEVIRT_PREINBOUND",
			"-j",
			"DNAT",
			"--to-destination", p.getVifIpByProtocol(protocol))

		return err
	}

	for _, port := range p.iface.Ports {
		if port.Protocol == "" {
			port.Protocol = "tcp"
		}

		err = Handler.IptablesAppendRule(protocol, "nat", "KUBEVIRT_POSTINBOUND",
			"-p",
			strings.ToLower(port.Protocol),
			"--dport",
			strconv.Itoa(int(port.Port)),
			"--source", getLoopbackAdrress(protocol),
			"-j",
			"SNAT",
			"--to-source", p.getGatewayByProtocol(protocol))
		if err != nil {
			return err
		}

		err = Handler.IptablesAppendRule(protocol, "nat", "KUBEVIRT_PREINBOUND",
			"-p",
			strings.ToLower(port.Protocol),
			"--dport",
			strconv.Itoa(int(port.Port)),
			"-j",
			"DNAT",
			"--to-destination", p.getVifIpByProtocol(protocol))
		if err != nil {
			return err
		}

		err = Handler.IptablesAppendRule(protocol, "nat", "OUTPUT",
			"-p",
			strings.ToLower(port.Protocol),
			"--dport",
			strconv.Itoa(int(port.Port)),
			"--destination", getLoopbackAdrress(protocol),
			"-j",
			"DNAT",
			"--to-destination", p.getVifIpByProtocol(protocol))
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *MasqueradePodInterface) getGatewayByProtocol(proto iptables.Protocol) string {
	if proto == iptables.ProtocolIPv4 {
		return p.gatewayAddr.IP.String()
	} else {
		return p.gatewayIpv6Addr.IP.String()
	}
}

func (p *MasqueradePodInterface) getVifIpByProtocol(proto iptables.Protocol) string {
	if proto == iptables.ProtocolIPv4 {
		return p.vif.IP.IP.String()
	} else {
		return p.vif.IPv6.IP.String()
	}
}

func getLoopbackAdrress(proto iptables.Protocol) string {
	if proto == iptables.ProtocolIPv4 {
		return "127.0.0.1"
	} else {
		return "::1"
	}
}

func (p *MasqueradePodInterface) createNatRulesUsingNftables(proto iptables.Protocol) error {
	err := Handler.NftablesNewChain(proto, "nat", "KUBEVIRT_PREINBOUND")
	if err != nil {
		return err
	}

	err = Handler.NftablesNewChain(proto, "nat", "KUBEVIRT_POSTINBOUND")
	if err != nil {
		return err
	}

	err = Handler.NftablesAppendRule(proto, "nat", "postrouting", Handler.GetNFTIPString(proto), "saddr", p.getVifIpByProtocol(proto), "counter", "masquerade")
	if err != nil {
		return err
	}

	err = Handler.NftablesAppendRule(proto, "nat", "prerouting", "iifname", p.podInterfaceName, "counter", "jump", "KUBEVIRT_PREINBOUND")
	if err != nil {
		return err
	}

	err = Handler.NftablesAppendRule(proto, "nat", "postrouting", "oifname", p.bridgeInterfaceName, "counter", "jump", "KUBEVIRT_POSTINBOUND")
	if err != nil {
		return err
	}

	if len(p.iface.Ports) == 0 {
		err = Handler.NftablesAppendRule(proto, "nat", "KUBEVIRT_PREINBOUND",
			"counter", "dnat", "to", p.getVifIpByProtocol(proto))

		return err
	}

	for _, port := range p.iface.Ports {
		if port.Protocol == "" {
			port.Protocol = "tcp"
		}

		err = Handler.NftablesAppendRule(proto, "nat", "KUBEVIRT_POSTINBOUND",
			strings.ToLower(port.Protocol),
			"dport",
			strconv.Itoa(int(port.Port)),
			Handler.GetNFTIPString(proto), "saddr", getLoopbackAdrress(proto),
			"counter", "snat", "to", p.getGatewayByProtocol(proto))
		if err != nil {
			return err
		}

		err = Handler.NftablesAppendRule(proto, "nat", "KUBEVIRT_PREINBOUND",
			strings.ToLower(port.Protocol),
			"dport",
			strconv.Itoa(int(port.Port)),
			"counter", "dnat", "to", p.getVifIpByProtocol(proto))
		if err != nil {
			return err
		}

		err = Handler.NftablesAppendRule(proto, "nat", "output",
			Handler.GetNFTIPString(proto), "daddr", getLoopbackAdrress(proto),
			strings.ToLower(port.Protocol),
			"dport",
			strconv.Itoa(int(port.Port)),
			"counter", "dnat", "to", p.getVifIpByProtocol(proto))
		if err != nil {
			return err
		}
	}

	return nil
}

type SlirpPodInterface struct {
	vmi       *v1.VirtualMachineInstance
	iface     *v1.Interface
	virtIface *api.Interface
	domain    *api.Domain
}

func (s *SlirpPodInterface) discoverPodNetworkInterface() error {
	return nil
}

func (s *SlirpPodInterface) preparePodNetworkInterfaces(queueNumber uint32, launcherPID int) error {
	return nil
}

func (s *SlirpPodInterface) startDHCP(vmi *v1.VirtualMachineInstance) error {
	return nil
}

func (s *SlirpPodInterface) decorateConfig() error {
	// remove slirp interface from domain spec devices interfaces
	var foundIface *api.Interface
	ifaces := s.domain.Spec.Devices.Interfaces
	for i, iface := range ifaces {
		if iface.Alias.Name == s.iface.Name {
			s.domain.Spec.Devices.Interfaces = append(ifaces[:i], ifaces[i+1:]...)
			foundIface = &iface
			break
		}
	}

	if foundIface == nil {
		return fmt.Errorf("failed to find interface %s in vmi spec", s.iface.Name)
	}

	qemuArg := fmt.Sprintf("%s,netdev=%s,id=%s", foundIface.Model.Type, s.iface.Name, s.iface.Name)
	if s.iface.MacAddress != "" {
		// We assume address was already validated in API layer so just pass it to libvirt as-is.
		qemuArg += fmt.Sprintf(",mac=%s", s.iface.MacAddress)
	}
	// Add interface configuration to qemuArgs
	s.domain.Spec.QEMUCmd.QEMUArg = append(s.domain.Spec.QEMUCmd.QEMUArg, api.Arg{Value: "-device"})
	s.domain.Spec.QEMUCmd.QEMUArg = append(s.domain.Spec.QEMUCmd.QEMUArg, api.Arg{Value: qemuArg})

	return nil
}

func (s *SlirpPodInterface) loadCachedInterface(pid, name string) (bool, error) {
	return true, nil
}

func (s *SlirpPodInterface) loadCachedVIF(pid, name string) (bool, error) {
	return true, nil
}

func (b *SlirpPodInterface) setCachedVIF(pid, name string) error {
	return nil
}

func (s *SlirpPodInterface) setCachedInterface(pid, name string) error {
	return nil
}

type MacvtapPodInterface struct {
	vmi              *v1.VirtualMachineInstance
	vif              *VIF
	iface            *v1.Interface
	virtIface        *api.Interface
	domain           *api.Domain
	podInterfaceName string
	podNicLink       netlink.Link
}

func (m *MacvtapPodInterface) discoverPodNetworkInterface() error {
	link, err := Handler.LinkByName(m.podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get a link for interface: %s", m.podInterfaceName)
		return err
	}
	m.podNicLink = link

	if len(m.vif.MAC) == 0 {
		// Get interface MAC address
		mac, err := Handler.GetMacDetails(m.podInterfaceName)
		if err != nil {
			log.Log.Reason(err).Errorf("failed to get MAC for %s", m.podInterfaceName)
			return err
		}
		m.vif.MAC = mac
	}

	return nil
}

func (m *MacvtapPodInterface) preparePodNetworkInterfaces(queueNumber uint32, launcherPID int) error {
	m.virtIface.MAC = &api.MAC{MAC: m.vif.MAC.String()}
	m.virtIface.MTU = &api.MTU{Size: strconv.Itoa(m.podNicLink.Attrs().MTU)}
	m.virtIface.Target = &api.InterfaceTarget{
		Device:  m.podInterfaceName,
		Managed: "no",
	}
	return nil
}

func (m *MacvtapPodInterface) decorateConfig() error {
	ifaces := m.domain.Spec.Devices.Interfaces
	for i, iface := range ifaces {
		if iface.Alias.Name == m.iface.Name {
			ifaces[i].MTU = m.virtIface.MTU
			ifaces[i].MAC = &api.MAC{MAC: m.vif.MAC.String()}
			ifaces[i].Target = m.virtIface.Target
			break
		}
	}
	return nil
}

func (m *MacvtapPodInterface) loadCachedInterface(uid, name string) (bool, error) {
	var ifaceConfig api.Interface

	isExist, err := readFromCachedFile(uid, name, interfaceCacheFile, &ifaceConfig)
	if err != nil {
		return false, err
	}

	if isExist {
		m.virtIface = &ifaceConfig
		return true, nil
	}

	return false, nil
}

func (m *MacvtapPodInterface) setCachedInterface(pid, name string) error {
	err := writeToCachedFile(m.virtIface, interfaceCacheFile, pid, name)
	return err
}

func (m *MacvtapPodInterface) loadCachedVIF(pid, name string) (bool, error) {
	buf, err := ioutil.ReadFile(getVifFilePath(pid, name))
	if err != nil {
		return false, err
	}
	err = json.Unmarshal(buf, &m.vif)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (m *MacvtapPodInterface) setCachedVIF(pid, name string) error {
	buf, err := json.MarshalIndent(&m.vif, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshaling vif object: %v", err)
	}
	return writeVifFile(buf, pid, name)
}

func (m *MacvtapPodInterface) startDHCP(vmi *v1.VirtualMachineInstance) error {
	// macvtap will connect to the host's subnet
	return nil
}

func createAndBindTapToBridge(virtualInterface *VIF, deviceName string, bridgeIfaceName string, queueNumber uint32, launcherPID int, mtu int) error {
	err := Handler.CreateTapDevice(deviceName, queueNumber, launcherPID, mtu)
	if err != nil {
		return err
	}
	virtualInterface.TapDevice = deviceName
	return Handler.BindTapDeviceToBridge(deviceName, bridgeIfaceName)
}

func generateTapDeviceName(podInterfaceName string) string {
	return "tap" + podInterfaceName[3:]
}
