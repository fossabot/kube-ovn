package ipam

import (
	"errors"
	"net"
	"strings"
	"sync"

	kubeovnv1 "github.com/alauda/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/alauda/kube-ovn/pkg/util"
	"k8s.io/klog"
)

var (
	OutOfRangeError  = errors.New("AddressOutOfRange")
	ConflictError    = errors.New("AddressConflict")
	NoAvailableError = errors.New("NoAvailableAddress")
	InvalidCIDRError = errors.New("CIDRInvalid")
)

type IPAM struct {
	mutex   sync.RWMutex
	Subnets map[string]*Subnet
}

type SubnetAddress struct {
	Subnet *Subnet
	Ip     string
	Mac    string
}

func NewIPAM() *IPAM {
	return &IPAM{
		mutex:   sync.RWMutex{},
		Subnets: map[string]*Subnet{},
	}
}

func (ipam *IPAM) GetRandomAddress(podName string, subnetName string) (string, string, string, error) {
	ipam.mutex.RLock()
	defer ipam.mutex.RUnlock()
	if subnet, ok := ipam.Subnets[subnetName]; !ok {
		return "", "", "", NoAvailableError
	} else {
		v4IP, v6IP, mac, err := subnet.GetRandomAddress(podName)
		klog.Infof("allocate v4 %s v6 %s mac %s for %s", v4IP, v6IP, mac, podName)
		return string(v4IP), string(v6IP), mac, err
	}
}

func (ipam *IPAM) GetStaticAddress(podName string, ip, mac string, subnetName string) (string, string, string, error) {
	ipam.mutex.RLock()
	defer ipam.mutex.RUnlock()
	if subnet, ok := ipam.Subnets[subnetName]; !ok {
		return "", "", "", NoAvailableError
	} else {
		protocol := util.CheckProtocol(ip)
		if protocol == kubeovnv1.ProtocolDual {
			ips := strings.Split(ip, ",")
			v4IP, mac, err := subnet.GetStaticAddress(podName, IP(ips[0]), mac, false)
			v6IP, mac, err := subnet.GetStaticAddress(podName, IP(ips[1]), mac, false)
			klog.Infof("allocate v4 %s v6 %s mac %s for pod %s", v4IP, v6IP, mac, podName)
			return string(v4IP), string(v6IP), mac, err
		} else {
			ip, mac, err := subnet.GetStaticAddress(podName, IP(ip), mac, false)
			klog.Infof("allocate %s %s for %s", ip, mac, podName)
			if protocol == kubeovnv1.ProtocolIPv4 {
				return string(ip), "", mac, err
			} else {
				return "", string(ip), mac, err
			}
		}
	}
}

func (ipam *IPAM) ReleaseAddressByPod(podName string) {
	ipam.mutex.RLock()
	defer ipam.mutex.RUnlock()
	for _, subnet := range ipam.Subnets {
		subnet.ReleaseAddress(podName)
	}
	return
}

func (ipam *IPAM) AddOrUpdateSubnet(name, cidrStr string, excludeIps []string) error {
	ipam.mutex.Lock()
	defer ipam.mutex.Unlock()
	v4cidrStr := cidrStr
	v6cidrStr := cidrStr
	var err error

	protocol := util.CheckProtocol(cidrStr)
	if protocol == kubeovnv1.ProtocolDual {
		v4cidrStr, v6cidrStr, err = util.CheckDualCidrs(cidrStr)
	} else {
		_, _, err = net.ParseCIDR(cidrStr)
	}
	if err != nil {
		return InvalidCIDRError
	}

	// subnet.Spec.ExcludeIps contains both v4 and v6 addresses
	v4ExcludeIps, v6ExcludeIps := splitIpsByProtocol(excludeIps)

	if subnet, ok := ipam.Subnets[name]; ok {
		subnet.Protocol = protocol
		if protocol == kubeovnv1.ProtocolDual || protocol == kubeovnv1.ProtocolIPv4 {
			subnet.V4ReservedIPList = convertExcludeIps(v4ExcludeIps)
			firstIP, _ := util.FirstSubnetIP(v4cidrStr)
			lastIP, _ := util.LastIP(v4cidrStr)
			subnet.V4FreeIPList = IPRangeList{&IPRange{Start: IP(firstIP), End: IP(lastIP)}}
			subnet.joinFreeWithReserve()
			for podName, ip := range subnet.V4PodToIP {
				mac := subnet.PodToMac[podName]
				if _, _, err := subnet.GetStaticAddress(podName, ip, mac, true); err != nil {
					klog.Errorf("%s address not in subnet %s new cidr %s", podName, name, cidrStr)
				}
			}
		}
		if protocol == kubeovnv1.ProtocolDual || protocol == kubeovnv1.ProtocolIPv6 {
			subnet.V6ReservedIPList = convertExcludeIps(v6ExcludeIps)
			firstIP, _ := util.FirstSubnetIP(v6cidrStr)
			lastIP, _ := util.LastIP(v6cidrStr)
			subnet.V6FreeIPList = IPRangeList{&IPRange{Start: IP(firstIP), End: IP(lastIP)}}
			subnet.joinFreeWithReserve()
			for podName, ip := range subnet.V6PodToIP {
				mac := subnet.PodToMac[podName]
				if _, _, err := subnet.GetStaticAddress(podName, ip, mac, true); err != nil {
					klog.Errorf("%s address not in subnet %s new cidr %s", podName, name, cidrStr)
				}
			}
		}
		return nil
	}

	subnet, err := NewSubnet(name, cidrStr, excludeIps)
	if err != nil {
		return err
	}
	klog.Infof("adding new subnet %s", name)
	ipam.Subnets[name] = subnet
	return nil
}

func (ipam *IPAM) DeleteSubnet(subnetName string) {
	ipam.mutex.Lock()
	defer ipam.mutex.Unlock()
	klog.Infof("delete subnet %s", subnetName)
	delete(ipam.Subnets, subnetName)
}

func (ipam *IPAM) GetPodAddress(podName string) []*SubnetAddress {
	ipam.mutex.RLock()
	defer ipam.mutex.RUnlock()
	addresses := []*SubnetAddress{}
	for _, subnet := range ipam.Subnets {
		v4IP, v6IP, mac, protocol := subnet.GetPodAddress(podName)
		if protocol == kubeovnv1.ProtocolIPv4 {
			addresses = append(addresses, &SubnetAddress{Subnet: subnet, Ip: string(v4IP), Mac: mac})
		} else if protocol == kubeovnv1.ProtocolIPv6 {
			addresses = append(addresses, &SubnetAddress{Subnet: subnet, Ip: string(v6IP), Mac: mac})
		} else {
			addresses = append(addresses, &SubnetAddress{Subnet: subnet, Ip: string(v4IP), Mac: mac})
			addresses = append(addresses, &SubnetAddress{Subnet: subnet, Ip: string(v6IP), Mac: mac})
		}
	}
	return addresses
}

func (ipam *IPAM) ContainAddress(address string) bool {
	ipam.mutex.RLock()
	defer ipam.mutex.RUnlock()
	for _, subnet := range ipam.Subnets {
		if subnet.ContainAddress(IP(address)) {
			return true
		}
	}
	return false
}

func (ipam *IPAM) SplitIpsByProtocol(excludeIps []string) ([]string, []string) {
	return splitIpsByProtocol(excludeIps)
}
