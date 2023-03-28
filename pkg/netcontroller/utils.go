package netcontroller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os/exec"
	"strconv"
	"strings"

	"github.com/nokia/net-attach-def-admission-controller/pkg/datatypes"

	"github.com/safchain/ethtool"
	"github.com/vishvananda/netlink"

	"k8s.io/klog"
)

const (
	sriovConfigFile = "/etc/pcidp/config.json"
)

type SriovResourceList struct {
	Resources []SriovResource `json:"resourceList"`
}

type SriovResource struct {
	ResourceName string         `json:"resourceName"`
	Selectors    SriovSelectors `json:"selectors"`
}

type SriovSelectors struct {
	PCIAddresses []string `json:"pciAddresses,omitempty"`
	RootDevices  []string `json:"rootDevices,omitempty"`
}

func getVlanInterface(vlanIfName string) bool {
	m := strings.Split(vlanIfName, ".")
	if len(m) != 2 {
		return false
	}
	if m[0] != "tenant" && m[0] != "provider" {
		return false
	}
	_, err := strconv.Atoi(m[1])
	if err != nil {
		return false
	}
	_, err = netlink.LinkByName(vlanIfName)
	if err != nil {
		return false
	}
	return true
}

func createVlanInterface(vlanMap map[string][]string, nadName string, vlanIfName string) (int, error) {
	m := strings.Split(vlanIfName, ".")
	// Check if vlan interface is created by other function
	vlanByOther := "vlan" + m[1]
	link, err := netlink.LinkByName(vlanByOther)
	if err == nil {
		parent, err := netlink.LinkByIndex(link.Attrs().ParentIndex)
		if err == nil {
			if parent.Attrs().Name == m[0]+"-bond" {
				klog.Infof("requested vlan is created by other function with name %s", vlanByOther)
				datatypes.AddToVlanMap(vlanMap, "other/"+vlanByOther, vlanIfName)
				// Check if vlan interface altname for self is already created
				if getVlanInterface(vlanIfName) {
					return datatypes.AddToVlanMap(vlanMap, nadName, vlanIfName), nil
				}
				cmd := exec.Command("/usr/sbin/ip", "link", "property", "add", "dev", vlanByOther, "altname", vlanIfName)
				err := cmd.Run()
				if err != nil {
					errString := fmt.Sprintf("add altname %s to %s failed: %s", vlanIfName, vlanByOther, err.Error())
					return 1, errors.New(errString)
				}
				return datatypes.AddToVlanMap(vlanMap, nadName, vlanIfName), nil
			}
		}
	}
	// Check if vlan interface is already created by self
	if getVlanInterface(vlanIfName) {
		klog.Infof("requested vlan interface %s is already created", vlanIfName)
		return datatypes.AddToVlanMap(vlanMap, nadName, vlanIfName), nil
	}
	// Check if master exists
	link, err = netlink.LinkByName(m[0] + "-bond")
	if err != nil {
		return 0, err
	}
	// Create the vlan interface
	vlan := netlink.Vlan{}
	vlan.ParentIndex = link.Attrs().Index
	vlan.Name = vlanIfName
	vlanId, _ := strconv.Atoi(m[1])
	vlan.VlanId = vlanId
	err = netlink.LinkAdd(&vlan)
	if err != nil {
		return 0, err
	}
	// Bring up the vlan interface
	err = netlink.LinkSetUp(&vlan)
	if err != nil {
		klog.Errorf("Failed at bring up %s: %s", vlanIfName, err.Error())
	}
	return datatypes.AddToVlanMap(vlanMap, vlanIfName, nadName), nil
}

func deleteVlanInterface(vlanMap map[string][]string, nadName string, vlanIfName string) (int, error) {
	m := strings.Split(vlanIfName, ".")
	// Check if vlan interface is created by other function
	vlanByOther := "vlan" + m[1]
	link, err := netlink.LinkByName(vlanByOther)
	if err == nil {
		parent, err := netlink.LinkByIndex(link.Attrs().ParentIndex)
		if err == nil {
			if parent.Attrs().Name == m[0]+"-bond" {
				klog.Infof("requested vlan is created by other function with name %s", vlanByOther)
				datatypes.AddToVlanMap(vlanMap, "other/"+vlanByOther, vlanIfName)
			}
		}
	}
	numUsers := datatypes.DelFromVlanMap(vlanMap, nadName, vlanIfName)
	if numUsers > 0 {
		return numUsers, nil
	}
	// Check if vlan interface exists
	link, err = netlink.LinkByName(vlanIfName)
	if err == nil {
		// Delete the vlan interface
		err = netlink.LinkDel(link)
	}
	return 0, err
}

func getNodeTopology(provider string) ([]byte, error) {
	topology := datatypes.NodeTopology{
		Bonds:      make(map[string]datatypes.NicMap),
		SriovPools: make(map[string]datatypes.NicMap),
	}

	pci2nic := make(map[string]datatypes.Nic)
	bondIndex := make(map[string]int)
	bondIndex["tenant-bond"] = 0
	bondIndex["provider-bond"] = 0
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	ethHandle, err := ethtool.NewEthtool()
	if err != nil {
		return nil, err
	}
	defer ethHandle.Close()
	for _, link := range links {
		bondName := ""
		if link.Attrs().Name == "tenant-bond" {
			bondName = "tenant-bond"
		} else if link.Attrs().Name == "provider-bond" {
			bondName = "provider-bond"
		}
		if bondName != "" {
			bondIndex[bondName] = link.Attrs().Index
			topology.Bonds[bondName] = make(datatypes.NicMap)
		}
	}
	for _, link := range links {
		macAddress, err := ethHandle.PermAddr(link.Attrs().Name)
		if err != nil {
			return nil, err
		}
		if provider == "openstack" {
			if !strings.HasPrefix(link.Attrs().Name, "eth") {
				continue
			}
		} else {
			if len(link.Attrs().Vfs) == 0 {
				continue
			}
		}
		pciAddress, err := ethHandle.BusInfo(link.Attrs().Name)
		if err != nil {
			return nil, err
		}
		nic := datatypes.Nic{
			Name:       link.Attrs().Name,
			MacAddress: macAddress}
		pci2nic[pciAddress] = nic
		bondName := ""
		if bondIndex["tenant-bond"] > 0 && link.Attrs().MasterIndex == bondIndex["tenant-bond"] {
			bondName = "tenant-bond"
		} else if bondIndex["provider-bond"] > 0 && link.Attrs().MasterIndex == bondIndex["provider-bond"] {
			bondName = "provider-bond"
		}
		if bondName != "" {
			var tmp []byte
			tmp, _ = json.Marshal(nic)
			var jsonNic datatypes.JsonNic
			json.Unmarshal(tmp, &jsonNic)
			if provider == "openstack" {
				topology.Bonds[bondName][nic.MacAddress] = jsonNic
			} else {
				topology.Bonds[bondName][nic.Name] = jsonNic
			}
		}
	}

	file, err := ioutil.ReadFile(sriovConfigFile)
	if err != nil {
		klog.Errorf("Error when getting sriovdp config file %s", sriovConfigFile)
	} else {
		var resourceList SriovResourceList
		err := json.Unmarshal(file, &resourceList)
		if err != nil {
			klog.Errorf("Error when reading sriovdp config file %s", sriovConfigFile)
		} else {
			for _, resource := range resourceList.Resources {
				topology.SriovPools[resource.ResourceName] = make(datatypes.NicMap)
				pciAddresses := []string{}
				if provider == "openstack" {
					pciAddresses = resource.Selectors.PCIAddresses
				} else {
					pciAddresses = resource.Selectors.RootDevices
				}
				for _, pciAddress := range pciAddresses {
					nic, ok := pci2nic[pciAddress]
					if ok {
						var tmp []byte
						tmp, _ = json.Marshal(nic)
						var jsonNic datatypes.JsonNic
						json.Unmarshal(tmp, &jsonNic)
						if provider == "openstack" {
							topology.SriovPools[resource.ResourceName][nic.MacAddress] = jsonNic
						} else {
							topology.SriovPools[resource.ResourceName][nic.Name] = jsonNic
						}
					}
				}
			}
		}
	}

	jsonTopology, err := json.Marshal(topology)
	return jsonTopology, err
}
