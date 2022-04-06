package vlanprovider

import (
	"fmt"
)

type NodeTopology struct {
	// bond -> nic
	Bonds map[string]map[string]map[string]interface{}
	// pool -> nic
	SriovPools map[string]map[string]map[string]interface{}
}

type Nic struct {
	Name       string `json:"name"`
	MacAddress string `json:"mac-address"`
}

type VlanProvider interface {
	Connect() error
	UpdateNodeTopology(string, string) (string, error)
	Attach(string, int, []string) error
	Detach(string, int, []string) error
}

func NewVlanProvider(provider string, config string) (VlanProvider, error) {
	switch provider {
	case "openstack":
		{
			openstack := &OpenstackVlanProvider{
				configFile: config}
			err := openstack.Connect()
			return openstack, err
		}
	case "baremetal":
		{
			fss := &FssVlanProvider{
				configFile: config}
			err := fss.Connect()
			return fss, err
		}
	default:
		return nil, fmt.Errorf("Not supported provider: %q", provider)
	}

}
