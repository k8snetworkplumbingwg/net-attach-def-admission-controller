package topocontroller

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"

	netattachdef "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"github.com/nokia/net-attach-def-admission-controller/pkg/datatypes"
)

func (c *TopologyController) getChangedNodes(oldNad, newNad *netattachdef.NetworkAttachmentDefinition) ([]corev1.Node, []corev1.Node, error) {
	var nodesToDetach, nodesToAttach []corev1.Node
	// Get old nodes
	oldAnnotations := oldNad.GetAnnotations()
	oldLS, _ := oldAnnotations[datatypes.NodeSelectorKey]
	oldNodes, err := c.k8sClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: oldLS})
	if err != nil {
		return nodesToDetach, nodesToAttach, err
	}
	var oldNames []string
	for _, node := range oldNodes.Items {
		oldNames = append(oldNames, node.ObjectMeta.Name)
	}
	// Get new nodes
	newAnnotations := newNad.GetAnnotations()
	newLS, _ := newAnnotations[datatypes.NodeSelectorKey]
	newNodes, err := c.k8sClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: newLS})
	if err != nil {
		return nodesToDetach, nodesToAttach, err
	}
	var newNames []string
	for _, node := range newNodes.Items {
		newNames = append(newNames, node.ObjectMeta.Name)
	}
	// Get changed nodes
	m := make(map[string]int)
	for _, node := range oldNames {
		m[node] = 1
	}
	for _, node := range newNames {
		m[node] = m[node] + 1
	}
	var changedNodes []string
	for k, v := range m {
		if v == 1 {
			changedNodes = append(changedNodes, k)
		}
	}
	for _, cn := range changedNodes {
		for _, node := range oldNodes.Items {
			if cn == node.ObjectMeta.Name {
				nodesToDetach = append(nodesToDetach, node)
			}
		}
		for _, node := range newNodes.Items {
			if cn == node.ObjectMeta.Name {
				nodesToAttach = append(nodesToAttach, node)
			}
		}
	}
	return nodesToDetach, nodesToAttach, nil
}

func (c *TopologyController) updateNadAnnotations(nad *netattachdef.NetworkAttachmentDefinition, nodesAttached, nodesAttachFailed, nodesDetached []string) error {
	klog.Infof("updateNadAnnotations invoked for %s/%s", nad.ObjectMeta.Name, nad.ObjectMeta.Namespace)
	for i := 0; i < 256; i++ {
		nad, err := c.netAttachDefClientSet.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.ObjectMeta.Namespace).Get(context.TODO(), nad.ObjectMeta.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		annotationsMap := nad.GetAnnotations()
		var networkStatus NetworkStatus
		networkStatus = make(map[string][]string)
		jsonString, ok := annotationsMap[datatypes.NetworkStatusKey]
		if !ok || len(jsonString) == 0 {
			if len(nodesDetached) > 0 {
				return nil
			} else {
				networkStatus["attached"] = nodesAttached
				networkStatus["attachment-failed"] = nodesAttachFailed
			}
		} else {
			json.Unmarshal([]byte(jsonString), &networkStatus)
			if len(nodesAttached) > 0 {
				for _, n := range nodesAttached {
					nodeExists := false
					for _, v := range networkStatus["attached"] {
						if n == v {
							nodeExists = true
							break
						}
					}
					if !nodeExists {
						networkStatus["attached"] = append(networkStatus["attached"], n)
					}
					for k, v := range networkStatus["attachment-failed"] {
						if n == v {
							networkStatus["attachment-failed"] = append(networkStatus["attachment-failed"][:k], networkStatus["attachment-failed"][k+1:]...)
							break
						}
					}
				}
			}
			if len(nodesAttachFailed) > 0 {
				for _, n := range nodesAttachFailed {
					nodeExists := false
					for _, v := range networkStatus["attachment-failed"] {
						if n == v {
							nodeExists = true
							break
						}
					}
					if !nodeExists {
						networkStatus["attachment-failed"] = append(networkStatus["attachment-failed"], n)
					}
					for k, v := range networkStatus["attached"] {
						if n == v {
							networkStatus["attached"] = append(networkStatus["attached"][:k], networkStatus["attached"][k+1:]...)
							break
						}
					}
				}
			}
			if len(nodesDetached) > 0 {
				for _, n := range nodesDetached {
					for k, v := range networkStatus["attached"] {
						if n == v {
							networkStatus["attached"] = append(networkStatus["attached"][:k], networkStatus["attached"][k+1:]...)
							break
						}
					}
				}
				for _, n := range nodesDetached {
					for k, v := range networkStatus["attachment-failed"] {
						if n == v {
							networkStatus["attachment-failed"] = append(networkStatus["attachment-failed"][:k], networkStatus["attachment-failed"][k+1:]...)
							break
						}
					}
				}
			}
		}
		klog.Infof("Updated nodesAttached: %s", networkStatus["attached"])
		klog.Infof("Updated nodesAttachFailed: %s", networkStatus["attachment-failed"])
		updated, _ := json.Marshal(networkStatus)
		annotationsMap[datatypes.NetworkStatusKey] = string(updated)
		nad.SetAnnotations(annotationsMap)
		klog.V(3).Infof("Attempt: %d", i+1)
		_, err = c.netAttachDefClientSet.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.ObjectMeta.Namespace).Update(context.TODO(), nad, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			klog.Errorf("Update NAD annotaton failed because %s", err.Error())
			return err
		}
	}
	return nil
}

func (c *TopologyController) handleNetworkAttach(nad *netattachdef.NetworkAttachmentDefinition, nodes []corev1.Node, action datatypes.NadAction) error {
	name := nad.ObjectMeta.Name
	namespace := nad.ObjectMeta.Namespace
	klog.Infof("handleNetworkAttach invoked for %s/%s", namespace, name)

	netConf, _ := datatypes.GetNetConf(nad)
	annotationsMap := nad.GetAnnotations()
	project, _ := annotationsMap[datatypes.ExtProjectNameKey]
	network, _ := annotationsMap[datatypes.ExtNetworkNameKey]
	nodesAttached := []string{}
	nodesAttachFailed := []string{}
	nodesInfo := make(map[string]datatypes.NodeTopology)
	for _, node := range nodes {
		nodeName := node.ObjectMeta.Name
		nodeAnnotation := node.GetAnnotations()
		nodeInfo := datatypes.NodeTopology{
			Bonds:      make(map[string]datatypes.Bond),
			SriovPools: make(map[string]datatypes.NicMap),
		}
		topology, ok := nodeAnnotation[datatypes.NetworkTopologyKey]
		if !ok {
			klog.Errorf("Skip attaching %s: node topology is not available", nodeName)
			nodesAttachFailed = append(nodesAttachFailed, nodeName)
			continue
		}
		var nodeTopology datatypes.NodeTopology
		json.Unmarshal([]byte(topology), &nodeTopology)
		switch netConf.Type {
		case "ipvlan":
			{
				bondName := strings.Split(netConf.Master, ".")[0] + "-bond"
				bond, ok := nodeTopology.Bonds[bondName]
				if ok {
					nodeInfo.Bonds[bondName] = bond
				} else {
					klog.Errorf("Skip attaching %s: node topology is not available for bond %s", nodeName, bondName)
					nodesAttachFailed = append(nodesAttachFailed, nodeName)
					continue
				}
			}
		case "sriov":
			{
				resourceName, _ := annotationsMap[datatypes.SriovResourceKey]
				lastInd := strings.LastIndex(resourceName, "/")
				sriovPoolName := resourceName[lastInd+1:]
				nics, ok := nodeTopology.SriovPools[sriovPoolName]
				if ok {
					nodeInfo.SriovPools[sriovPoolName] = nics
				} else {
					klog.Errorf("Skip attaching %s: node topology is not available for sriov pool %s", nodeName, sriovPoolName)
					nodesAttachFailed = append(nodesAttachFailed, nodeName)
					continue
				}
			}
		}
		nodesInfo[nodeName] = nodeInfo
	}
	if len(nodesInfo) == 0 {
		klog.Infof("Skip the ATTACH procedure: no candidate node found for %s/%s", namespace, name)
		return nil
	}

	var overlays []map[string]string
	if netConf.Type == "sriov" && len(netConf.VlanTrunk) > 0 {
		jsonOverlays, _ := annotationsMap[datatypes.SriovOverlaysKey]
		json.Unmarshal([]byte(jsonOverlays), &overlays)
	} else {
		overlay := map[string]string{"extProjectName": project, "extNetworkName": network, "vlanRange": strconv.Itoa(netConf.Vlan)}
		overlays = append(overlays, overlay)
	}
	nodesError := make(map[string]error)
	for k, _ := range nodesInfo {
		nodesError[k] = nil
	}
	klog.Infof("Handle overlay %+v", overlays)
	for _, overlay := range overlays {
		project := overlay["extProjectName"]
		network := overlay["extNetworkName"]
		vlanRange := overlay["vlanRange"]
		nodesStatus, err := c.vlanProvider.Attach(project, network, vlanRange, nodesInfo, action)
		if err != nil {
			klog.Errorf("Plugin Attach for vlan %s failed: %s", vlanRange, err.Error())
		}
		for k, v := range nodesStatus {
			if v != nil {
				nodesError[k] = v
			} else if err != nil {
				nodesError[k] = err
			}
		}
	}

	for k, v := range nodesError {
		if v == nil {
			nodesAttached = append(nodesAttached, k)
		} else {
			nodesAttachFailed = append(nodesAttachFailed, k)
		}
	}
	nodesDetached := []string{}
	c.updateNadAnnotations(nad, nodesAttached, nodesAttachFailed, nodesDetached)
	return nil
}

func (c *TopologyController) handleNetworkDetach(nad *netattachdef.NetworkAttachmentDefinition, nodes []corev1.Node, action datatypes.NadAction) error {
	name := nad.ObjectMeta.Name
	namespace := nad.ObjectMeta.Namespace
	klog.Infof("handleNetworkDetach invoked for %s/%s", namespace, name)

	netConf, _ := datatypes.GetNetConf(nad)
	annotationsMap := nad.GetAnnotations()
	project, _ := annotationsMap[datatypes.ExtProjectNameKey]
	network, _ := annotationsMap[datatypes.ExtNetworkNameKey]
	var nodesDetached []string
        nodesInfo := make(map[string]datatypes.NodeTopology)
	for _, node := range nodes {
		nodeName := node.ObjectMeta.Name
                nodeInfo := datatypes.NodeTopology{
                        Bonds:      make(map[string]datatypes.Bond),
                        SriovPools: make(map[string]datatypes.NicMap),
                }
		nodesDetached = append(nodesDetached, nodeName)
		nodeAnnotation := node.GetAnnotations()
		topology, ok := nodeAnnotation[datatypes.NetworkTopologyKey]
		if !ok {
			klog.Errorf("Skip detaching %s: node topology is not available", nodeName)
			continue
		}
		var nodeTopology datatypes.NodeTopology
		json.Unmarshal([]byte(topology), &nodeTopology)
		switch netConf.Type {
		case "ipvlan":
			{
				bondName := strings.Split(netConf.Master, ".")[0] + "-bond"
				bond, ok := nodeTopology.Bonds[bondName]
				if !ok {
					klog.Errorf("Skip detaching %s: node topology is not available for bond %s", nodeName, bondName)
					continue
				}
                                nodeInfo.Bonds[bondName] = bond
			}
		case "sriov":
			{
				resourceName, _ := annotationsMap[datatypes.SriovResourceKey]
				lastInd := strings.LastIndex(resourceName, "/")
				sriovPoolName := resourceName[lastInd+1:]
				nics, ok := nodeTopology.SriovPools[sriovPoolName]
				if !ok {
					klog.Errorf("Skip detaching %s: node topology is not available for sriov pool %s", nodeName, sriovPoolName)
					continue
				}
                                nodeInfo.SriovPools[sriovPoolName] = nics
			}
		}
                nodesInfo[nodeName] = nodeInfo
	}
	if action == datatypes.UpdateDetach && len(nodesInfo) == 0 {
		klog.Infof("Skip the DETACH procedure: no candidate node found for %s/%s", namespace, name)
		return nil
	}

	var overlays []map[string]string
	if netConf.Type == "sriov" && len(netConf.VlanTrunk) > 0 {
		jsonOverlays, _ := annotationsMap[datatypes.SriovOverlaysKey]
		json.Unmarshal([]byte(jsonOverlays), &overlays)
	} else {
		overlay := map[string]string{"extProjectName": project, "extNetworkName": network, "vlanRange": strconv.Itoa(netConf.Vlan)}
		overlays = append(overlays, overlay)
	}
	klog.Infof("Handle overlay %+v", overlays)
	for _, v := range overlays {
		project := v["extProjectName"]
		network := v["extNetworkName"]
		vlanRange := v["vlanRange"]
		_, err := c.vlanProvider.Detach(project, network, vlanRange, nodesInfo, action)
		if err != nil {
			klog.Errorf("Plugin Detach for vlan %s failed: %s", vlanRange, err.Error())
		}
	}

	if action != datatypes.DeleteDetach {
		nodesAttached := []string{}
		nodesAttachFailed := []string{}
		c.updateNadAnnotations(nad, nodesAttached, nodesAttachFailed, nodesDetached)
	}
	return nil
}

func (c *TopologyController) processNadItem(workItem WorkItem) error {
	klog.Infof("processNadItem invoked for %s/%s", workItem.newNad.ObjectMeta.Name, workItem.newNad.ObjectMeta.Namespace)
	var err error
	// Note: no return inside the switch block
	switch workItem.action {
	case datatypes.CreateAttach, datatypes.UpdateAttach:
		{
			annotationsMap := workItem.newNad.GetAnnotations()
			ns, _ := annotationsMap[datatypes.NodeSelectorKey]
			nodes, err := c.k8sClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: ns})
			if err != nil {
				klog.Errorf("Get matching nodes failed: %s", err.Error())
				break
			}
			if len(nodes.Items) == 0 {
				klog.Infof("No matching node found")
				break
			}
			err = c.handleNetworkAttach(workItem.newNad, nodes.Items, workItem.action)
			if err != nil {
				klog.Errorf("handleNetworkAttach failed because %s", err.Error())
			}
		}
	case datatypes.DeleteDetach, datatypes.UpdateDetach:
		{
			annotationsMap := workItem.oldNad.GetAnnotations()
			ns, _ := annotationsMap[datatypes.NodeSelectorKey]
			nodes, err := c.k8sClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: ns})
			if err != nil {
				klog.Errorf("Get matching nodes failed: %s", err.Error())
				break
			}
			if workItem.action == datatypes.UpdateDetach && len(nodes.Items) == 0 {
				klog.Infof("No matching node found")
				break
			}
			err = c.handleNetworkDetach(workItem.oldNad, nodes.Items, workItem.action)
			if err != nil {
				klog.Errorf("handleNetworkDetach failed because %s", err.Error())
			}
		}
	case datatypes.UpdateAttachDetach:
		{
			var networkStatus NetworkStatus
			networkStatus = make(map[string][]string)
			annotationsMap := workItem.oldNad.GetAnnotations()
			jsonString, ok := annotationsMap[datatypes.NetworkStatusKey]
			if ok && len(jsonString) > 0 {
				json.Unmarshal([]byte(jsonString), &networkStatus)
			}
			oldNodes, newNodes, _ := c.getChangedNodes(workItem.oldNad, workItem.newNad)
			var nodesToDetach, nodesToAttach []corev1.Node
			// detach
			if len(oldNodes) > 0 {
				for _, node := range oldNodes {
					attached := false
					for _, v := range networkStatus["attached"] {
						if v == node.ObjectMeta.Name {
							attached = true
							break
						}
					}
					if attached {
						nodesToDetach = append(nodesToDetach, node)
					}
				}
				if len(nodesToDetach) > 0 {
					err := c.handleNetworkDetach(workItem.oldNad, nodesToDetach, workItem.action)
					if err != nil {
						klog.Errorf("handleNetworkDetach failed because %s", err.Error())
					}
				}
			}
			// attach
			if len(newNodes) > 0 {
				for _, node := range newNodes {
					attached := false
					for _, v := range networkStatus["attached"] {
						if v == node.ObjectMeta.Name {
							attached = true
							break
						}
					}
					if !attached {
						nodesToAttach = append(nodesToAttach, node)
					}
				}
				if len(nodesToAttach) > 0 {
					err = c.handleNetworkAttach(workItem.newNad, nodesToAttach, workItem.action)
					if err != nil {
						klog.Errorf("handleNetworkAttach failed because %s", err.Error())
					}
				}
			}
		}
	}
	c.vlanProvider.TxnDone()
	return nil
}

func (c *TopologyController) processNodeItem(workItem WorkItem) error {
	klog.Infof("processNodeItem invoked for %s", workItem.node.ObjectMeta.Name)
	nadList, err := c.netAttachDefClientSet.K8sCniCncfIoV1().NetworkAttachmentDefinitions("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		klog.Errorf("List network attachment definitions failed because %s", err.Error())
		return err
	}
	// Note: no return inside the switch block
	switch workItem.action {
	case datatypes.NodeAttach:
		{
			nodeLabels := workItem.node.GetLabels()
			for _, nad := range nadList.Items {
				name := nad.ObjectMeta.Name
				namespace := nad.ObjectMeta.Namespace
				klog.Infof("Checking NAD %s/%s", namespace, name)
				_, trigger, _ := datatypes.ShouldTriggerTopoAction(&nad)
				if !trigger {
					continue
				}
				annotationsMap := nad.GetAnnotations()
				ns, _ := annotationsMap[datatypes.NodeSelectorKey]
				klog.Infof("nodeSelector: %s", ns)
				label := strings.Split(ns, "=")
				if len(label) != 2 {
					continue
				}
				v, ok := nodeLabels[label[0]]
				if !ok || v != label[1] {
					continue
				}
				klog.Infof("the node has the label: %s=%s", label[0], v)
				err = c.handleNetworkAttach(&nad, []corev1.Node{*workItem.node}, workItem.action)
				if err != nil {
					klog.Errorf("handleNetworkAttach failed because %s", err.Error())
				}
			}
		}
	case datatypes.NodeDetach:
		{
			nodeLabels := workItem.node.GetLabels()
			for _, nad := range nadList.Items {
				name := nad.ObjectMeta.Name
				namespace := nad.ObjectMeta.Namespace
				klog.Infof("Checking NAD %s/%s", namespace, name)
				_, trigger, _ := datatypes.ShouldTriggerTopoAction(&nad)
				if !trigger {
					continue
				}
				annotationsMap := nad.GetAnnotations()
				ns, _ := annotationsMap[datatypes.NodeSelectorKey]
				klog.Infof("nodeSelector: %s", ns)
				label := strings.Split(ns, "=")
				if len(label) != 2 {
					continue
				}
				v, ok := nodeLabels[label[0]]
				if !ok || v != label[1] {
					continue
				}
				klog.Infof("the node has the label: %s=%s", label[0], v)
				err = c.handleNetworkDetach(&nad, []corev1.Node{*workItem.node}, workItem.action)
				if err != nil {
					klog.Errorf("handleNetworkDetach failed because %s", err.Error())
				}
			}
			c.vlanProvider.DetachNode(workItem.node.ObjectMeta.Name)
		}
	case datatypes.NodeAttachDetach:
		{
			nodeLabels := workItem.node.GetLabels()
			for _, nad := range nadList.Items {
				name := nad.ObjectMeta.Name
				namespace := nad.ObjectMeta.Namespace
				klog.Infof("Checking NAD %s/%s", namespace, name)
				_, trigger, _ := datatypes.ShouldTriggerTopoAction(&nad)
				if !trigger {
					continue
				}
                                annotationsMap := nad.GetAnnotations()
                                ns, _ := annotationsMap[datatypes.NodeSelectorKey]
                                var networkStatus NetworkStatus
                                networkStatus = make(map[string][]string)
				jsonString, ok := annotationsMap[datatypes.NetworkStatusKey]
				if ok && len(jsonString) > 0 {
					json.Unmarshal([]byte(jsonString), &networkStatus)
				}
				attached := false
				for _, v := range networkStatus["attached"] {
					if v == workItem.node.ObjectMeta.Name {
						attached = true
						break
					}
				}
				klog.Infof("nodeSelector: %s", ns)
				label := strings.Split(ns, "=")
				if len(label) != 2 {
					continue
				}
				v, ok := nodeLabels[label[0]]
				if ok && v == label[1] {
					klog.Infof("the node has the label: %s=%s", label[0], v)
					if !attached {
						err = c.handleNetworkAttach(&nad, []corev1.Node{*workItem.node}, workItem.action)
						if err != nil {
							klog.Errorf("handleNetworkAttach failed because %s", err.Error())
						}
					}
				} else {
					klog.Infof("the node does not have the label: %s=%s", label[0], v)
					if attached {
						err = c.handleNetworkDetach(&nad, []corev1.Node{*workItem.node}, workItem.action)
						if err != nil {
							klog.Errorf("handleNetworkDetach failed because %s", err.Error())
						}
					}
				}
			}
		}
	}
	c.vlanProvider.TxnDone()
	return err
}
