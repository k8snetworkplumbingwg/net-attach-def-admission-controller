package vlanprovider

import (
	"fmt"
	"k8s.io/client-go/kubernetes"

	"github.com/nokia/net-attach-def-admission-controller/pkg/datatypes"
)

type VlanProvider interface {
	Connect(kubernetes.Interface, string) error
	UpdateNodeTopology(string, string) (string, error)
	Attach(string, string, string, map[string]datatypes.NicMap, datatypes.NadAction) (map[string]error, error)
	Detach(string, string, string, map[string]datatypes.NicMap, datatypes.NadAction) (map[string]error, error)
}

func NewVlanProvider(provider string, config string) (VlanProvider, error) {
	switch provider {
	case "openstack":
		{
			openstack := &OpenstackVlanProvider{
				configFile: config}
			return openstack, nil
		}
	case "baremetal":
		{
			fss := &FssVlanProvider{
				configFile: config}
			return fss, nil
		}
	default:
		return nil, fmt.Errorf("Not supported provider: %q", provider)
	}

}
