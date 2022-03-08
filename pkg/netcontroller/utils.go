package netcontroller

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"strings"

	"github.com/jaypipes/ghw"
	"github.com/nokia/net-attach-def-admission-controller/pkg/vlanprovider"
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
	Drivers      []string `json:"drivers,omitempty"`
	PFNames      []string `json:"pfNames,omitempty"`
}

func getVlanInterface(vlanIfName string) bool {
	_, err := netlink.LinkByName(vlanIfName)
	if err == nil {
		return true
	}
	return false
}

func createVlanInterface(vlanIfName string, vlanId int) error {
	// Check if vlan interface already exists
	if getVlanInterface(vlanIfName) {
		klog.Info("requested vlan interface already exists")
		return nil
	}
	m := strings.Split(vlanIfName, ".")
	// Check if vlanId is already used
	vlanByOther := "vlan" + m[1]
	_, err := netlink.LinkByName(vlanByOther)
	if err == nil {
		return errors.New("requested vlan is already used by other function")
	}
	// Check if master exists
	link, err := netlink.LinkByName(m[0])
	if err != nil {
		return err
	}
	// Create the vlan interface
	vlan := netlink.Vlan{}
	vlan.ParentIndex = link.Attrs().Index
	vlan.Name = vlanIfName
	vlan.VlanId = vlanId
	err = netlink.LinkAdd(&vlan)
	if err != nil {
		return err
	}
	err = netlink.LinkSetUp(&vlan)
	if err != nil {
		return err
	}
	return nil
}

func deleteVlanInterface(vlanIfName string) error {
	// Check if vlan interface exists
	link, err := netlink.LinkByName(vlanIfName)
	if err != nil {
		return nil
	}
	// Delete the vlan interface
	err = netlink.LinkDel(link)
	if err != nil {
		return err
	}
	return nil
}

func getNodeTopology(provider string) ([]byte, error) {
	topology := vlanprovider.NodeTopology{
		Bonds:      make(map[string]map[string]map[string]interface{}),
		SriovPools: make(map[string]map[string]map[string]interface{}),
	}

	name2nic := make(map[string]vlanprovider.Nic)
	pci2nic := make(map[string]vlanprovider.Nic)
	bondIndex := make(map[string]int)
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	for _, link := range links {
		bondName := ""
		if link.Attrs().Name == "tenant-bond" {
			bondName = "tenant-bond"
		} else if link.Attrs().Name == "provider-bond" {
			bondName = "provider-bond"
		}
		if bondName != "" {
			bondIndex[bondName] = link.Attrs().Index
			topology.Bonds[bondName] = make(map[string]map[string]interface{})
		}
	}
	for _, link := range links {
		nic := vlanprovider.Nic{
			Name:       link.Attrs().Name,
			MacAddress: link.Attrs().HardwareAddr.String()}
		name2nic[nic.Name] = nic
		bondName := ""
		if bondIndex["tenant-bond"] > 0 && link.Attrs().MasterIndex == bondIndex["tenant-bond"] {
			bondName = "tenant-bond"
		} else if bondIndex["provider-bond"] > 0 && link.Attrs().MasterIndex == bondIndex["provider-bond"] {
			bondName = "provider-bond"
		}
		if bondName != "" {
			var tmp []byte
			tmp, _ = json.Marshal(nic)
			var jsonString map[string]interface{}
			json.Unmarshal(tmp, &jsonString)
			topology.Bonds[bondName][link.Attrs().HardwareAddr.String()] = jsonString
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
			if provider == "openstack" {
				net, err := ghw.Network()
				if err != nil {
					return nil, err
				}
				for _, nic := range net.NICs {
					if nic.IsVirtual {
						continue
					}
					if strings.HasPrefix(nic.Name, "eth") {
						pci2nic[*nic.PCIAddress] = vlanprovider.Nic{
							Name:       nic.Name,
							MacAddress: nic.MacAddress}
					}
				}
			}
			for _, resource := range resourceList.Resources {
				topology.SriovPools[resource.ResourceName] = make(map[string]map[string]interface{})
				if provider == "openstack" {
					for _, pciAddress := range resource.Selectors.PCIAddresses {
						nic, ok := pci2nic[pciAddress]
						if ok {
							var tmp []byte
							tmp, _ = json.Marshal(nic)
							var jsonString map[string]interface{}
							json.Unmarshal(tmp, &jsonString)
							topology.SriovPools[resource.ResourceName][nic.MacAddress] = jsonString
						}
					}
				} else {
					for _, pfName := range resource.Selectors.PFNames {
						nic, ok := name2nic[pfName]
						if ok {
							var tmp []byte
							tmp, _ = json.Marshal(nic)
							var jsonString map[string]interface{}
							json.Unmarshal(tmp, &jsonString)
							topology.SriovPools[resource.ResourceName][nic.MacAddress] = jsonString
						}
					}
				}
			}
		}
	}

	jsonTopology, err := json.Marshal(topology)
	return jsonTopology, err
}
