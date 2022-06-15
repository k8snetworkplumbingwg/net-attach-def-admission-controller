package vlanprovider

import (
	"errors"
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
	fssConfig.Global.Restartmode = "cache"
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

func (p *FssVlanProvider) Attach(fssWorkloadEvpnId string, fssSubnetId string, vlanIds []int, nodesInfo map[string]datatypes.NicMap, requestType datatypes.NadAction) (map[string]error, error) {
	nodesStatus := make(map[string]error)
	for k, _ := range nodesInfo {
		nodesStatus[k] = errors.New("undefined")
	}
	for _, vlanId := range vlanIds {
		klog.Infof("Attach step 1: get hostPortLabel for vlan %d on fssWorkloadEvpnId %s fssSubnetId %s", vlanId, fssWorkloadEvpnId, fssSubnetId)
		hostPortLabelID, err := p.fssClient.CreateSubnetInterface(fssWorkloadEvpnId, fssSubnetId, vlanId)
		if err != nil {
			return nodesStatus, err
		}
		nodeCreated := false
		if requestType == datatypes.NodeAttach {
			nodeCreated = true
		}
		for k, v := range nodesInfo {
			for i, _ := range v {
				klog.Infof("Attach step 2a: attach hostPortLabel for vlan %d to host %s port %s", vlanId, k, i)
				err := p.fssClient.AttachHostPort(hostPortLabelID, k, i, nodeCreated)
				nodesStatus[k] = err
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
	p.fssClient.TxnDone()
	return nodesStatus, nil
}

func (p *FssVlanProvider) Detach(fssWorkloadEvpnId string, fssSubnetId string, vlanIds []int, nodesInfo map[string]datatypes.NicMap, requestType datatypes.NadAction) (map[string]error, error) {
	nodesStatus := make(map[string]error)
	for k, _ := range nodesInfo {
		nodesStatus[k] = errors.New("undefined")
	}
	for _, vlanId := range vlanIds {
		klog.Infof("Detach step 1: get hostPortLabel for vlan %d on fssWorkloadEvpnId %s fssSubnetId %s", vlanId, fssWorkloadEvpnId, fssSubnetId)
		hostPortLabelID, exists := p.fssClient.GetSubnetInterface(fssWorkloadEvpnId, fssSubnetId, vlanId)
		if !exists {
			return nodesStatus, fmt.Errorf("Reqeusted vlan %d does not exist", vlanId)
		}
		if requestType == datatypes.DeleteDetach || requestType == datatypes.UpdateDetach {
			klog.Infof("Detach step 2: delete vlan %d on fssSubnetId %s", vlanId, fssSubnetId)
			err := p.fssClient.DeleteSubnetInterface(fssSubnetId, vlanId, hostPortLabelID)
			if err != nil {
				return nodesStatus, err
			}
		} else {
			nodeDeleted := false
			if requestType == datatypes.NodeDetach {
				nodeDeleted = true
			}
			for k, v := range nodesInfo {
				for i, _ := range v {
					klog.Infof("Detach step 2a: detach vlan %d from host %s port %s", vlanId, k, i)
					err := p.fssClient.DetachHostPort(hostPortLabelID, k, i, nodeDeleted)
					nodesStatus[k] = err
				}
			}
		}
	}
	p.fssClient.TxnDone()
	return nodesStatus, nil
}
