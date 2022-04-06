package vlanprovider

import (
	"k8s.io/klog"
)

type FssVlanProvider struct {
	configFile string
}

func (p *FssVlanProvider) Connect() error {
	klog.Info("Fss: connect...")
	return nil
}

func (p *FssVlanProvider) UpdateNodeTopology(name string, topology string) (string, error) {
	return topology, nil
}

func (p *FssVlanProvider) Attach(network string, vlan int, nodes []string) error {
	klog.Infof("Fss: attach vlan %d to %s for %s", vlan, network, nodes)
	return nil
}

func (p *FssVlanProvider) Detach(network string, vlan int, nodes []string) error {
	klog.Infof("Fss: detach vlan %d from %s for %s", vlan, network, nodes)
	return nil
}
