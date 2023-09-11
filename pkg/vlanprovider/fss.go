package vlanprovider

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/nokia/net-attach-def-admission-controller/pkg/datatypes"
	client "github.com/nokia/net-attach-def-admission-controller/pkg/fssclient"
	gcfg "gopkg.in/gcfg.v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"
)

// FssConfig is used to read and store information from the FSS configuration file
type FssConfig struct {
	Global client.AuthOpts
}

type FssVlanProvider struct {
	configFile string
	fssClient  *client.FssClient
}

func (p *FssVlanProvider) Connect(k8sClientSet kubernetes.Interface, podNamespace string) error {
	// Read FSS Config
	f, err := os.Open(p.configFile)
	if err != nil {
		return err
	}
	defer f.Close()
	var fData io.Reader
	fData = f
	var fssConfig FssConfig
	fssConfig.Global.Restartmode = "resync"
	err = gcfg.FatalOnly(gcfg.ReadInto(&fssConfig, fData))
	if err != nil {
		return err
	}
	// Connect to FSS
	fssClient, err := client.NewFssClient(k8sClientSet, podNamespace, &fssConfig.Global)
	if err != nil {
		return err
	}
	p.fssClient = fssClient
	klog.Info("FSS: connected")
	return nil
}

func (p *FssVlanProvider) UpdateNodeTopology(name string, topology string) (string, error) {
	return topology, nil
}

func (p *FssVlanProvider) Attach(fssWorkloadEvpnName, fssSubnetName, vlanRange string, nodesInfo map[string]datatypes.NodeTopology, requestType datatypes.NadAction) (map[string]error, error) {
	nodesStatus := make(map[string]error)
	for k, _ := range nodesInfo {
		nodesStatus[k] = nil
	}
	vlanIds, _ := datatypes.GetVlanIds(vlanRange)
	for _, vlanId := range vlanIds {
		klog.Infof("Attach step 1: get hostPortLabel for vlan %d on fssWorkloadEvpnName %s fssSubnetName %s", vlanId, fssWorkloadEvpnName, fssSubnetName)
		fssSubnetId, hostPortLabelID, err := p.fssClient.CreateSubnetInterface(fssWorkloadEvpnName, fssSubnetName, vlanId)
		if err != nil {
			return nodesStatus, err
		}
		for nodeName, nodeTopology := range nodesInfo {
			for bondName, bond := range nodeTopology.Bonds {
				parentHostPortID := ""
				var err error
				if bond.Mode == "802.3ad" {
					nic := datatypes.Nic{
						Name:       bondName,
						MacAddress: bond.MacAddress}
					var tmp []byte
					tmp, _ = json.Marshal(nic)
					var jsonNic datatypes.JsonNic
					json.Unmarshal(tmp, &jsonNic)
					parentHostPortID, err = p.fssClient.CreateHostPort(nodeName, jsonNic, true, "")
					if err != nil {
						nodesStatus[nodeName] = err
						continue
					}
					klog.Infof("Node host %s Create Parent Port for LACP Bond %s", nodeName, bondName)
					for portName, port := range nodeTopology.Bonds[bondName].Ports {
						klog.Infof("Node host %s Create Slave Port %s", nodeName, portName)
						_, err := p.fssClient.CreateHostPort(nodeName, port, false, parentHostPortID)
						if err != nil {
							nodesStatus[nodeName] = err
							continue
						}
					}
					klog.Infof("Attach step 2a: attach hostPortLabel for vlan %d to host %s parent port %s", vlanId, nodeName, bondName)
					err := p.fssClient.AttachHostPort(hostPortLabelID, nodeName, jsonNic, "")
					if err != nil {
						nodesStatus[nodeName] = err
						continue
					}
				} else {
					for portName, port := range nodeTopology.Bonds[bondName].Ports {
						klog.Infof("Attach step 2a: attach hostPortLabel for vlan %d to host %s bond port %s", vlanId, nodeName, portName)
						err := p.fssClient.AttachHostPort(hostPortLabelID, nodeName, port, parentHostPortID)
						if err != nil {
							nodesStatus[nodeName] = err
							continue
						}
					}
				}
			}
			for k, v := range nodeTopology.SriovPools {
				for portName, port := range v {
					klog.Infof("Attach step 2a: attach hostPortLabel for vlan %d to host %s sriov port %s", vlanId, nodeName, portName)
					err := p.fssClient.AttachHostPort(hostPortLabelID, nodeName, port, "")
					if err != nil {
						nodesStatus[k] = err
						continue
					}
				}
			}
		}
		if requestType == datatypes.CreateAttach || requestType == datatypes.UpdateAttach {
			klog.Infof("Attach step 2: attach hostPortLabel vlan %d on fssSubnetId %s", vlanId, fssSubnetId)
			err = p.fssClient.AttachSubnetInterface(fssSubnetId, vlanId, hostPortLabelID)
			if err != nil {
				return nodesStatus, err
			}
		}
	}
	return nodesStatus, nil
}

func (p *FssVlanProvider) Detach(fssWorkloadEvpnName, fssSubnetName, vlanRange string, nodesInfo map[string]datatypes.NodeTopology, requestType datatypes.NadAction) (map[string]error, error) {
	nodesStatus := make(map[string]error)
	for nodeName, _ := range nodesInfo {
		nodesStatus[nodeName] = nil
	}
	vlanIds, _ := datatypes.GetVlanIds(vlanRange)
	for _, vlanId := range vlanIds {
		klog.Infof("Detach step 1: get hostPortLabel for vlan %d on fssWorkloadEvpnName %s fssSubnetName %s", vlanId, fssWorkloadEvpnName, fssSubnetName)
		fssWorkloadEvpnId, fssSubnetId, hostPortLabelID, exists := p.fssClient.GetSubnetInterface(fssWorkloadEvpnName, fssSubnetName, vlanId)
		if !exists {
			return nodesStatus, fmt.Errorf("Reqeusted vlan %d does not exist", vlanId)
		}
		if requestType == datatypes.DeleteDetach || requestType == datatypes.UpdateDetach {
			klog.Infof("Detach step 2: delete vlan %d on fssSubnetId %s", vlanId, fssSubnetId)
			err := p.fssClient.DeleteSubnetInterface(fssWorkloadEvpnId, fssSubnetId, vlanId, hostPortLabelID, requestType)
			if err != nil {
				return nodesStatus, err
			}
		} else {
			for nodeName, nodeTopology := range nodesInfo {
				for bondName, _ := range nodeTopology.Bonds {
					for portName, port := range nodeTopology.Bonds[bondName].Ports {
						klog.Infof("Detach step 2a: detach vlan %d from host %s bond port %s", vlanId, nodeName, portName)
						err := p.fssClient.DetachHostPort(hostPortLabelID, nodeName, port)
						nodesStatus[nodeName] = err
					}
				}
				for _, v := range nodeTopology.SriovPools {
					for portName, port := range v {
						klog.Infof("Detach step 2a: detach vlan %d from host %s sriov port %s", vlanId, nodeName, portName)
						err := p.fssClient.DetachHostPort(hostPortLabelID, nodeName, port)
						nodesStatus[nodeName] = err
					}
				}
			}
		}
	}
	return nodesStatus, nil
}

func (p *FssVlanProvider) DetachNode(nodeName string) {
	p.fssClient.DetachNode(nodeName)
}

func (p *FssVlanProvider) TxnDone() {
	p.fssClient.TxnDone()
}
