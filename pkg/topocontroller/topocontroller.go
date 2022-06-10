package topocontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	coreInformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	netattachdef "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	clientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	informers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"

	"github.com/nokia/net-attach-def-admission-controller/pkg/datatypes"
	"github.com/nokia/net-attach-def-admission-controller/pkg/vlanprovider"
)

const (
	controllerAgentName = "net-attach-def-topocontroller"
)

type PodInfo struct {
	NodeName       string
	PodNamespace   string
	Provider       string
	ProviderConfig string
}

type NetworkStatus map[string][]string

type WorkItem struct {
	action datatypes.NadAction
	oldNad *netattachdef.NetworkAttachmentDefinition
	newNad *netattachdef.NetworkAttachmentDefinition
	node   *corev1.Node
}

// TopologyController is the controller implementation for FSS Operator
type TopologyController struct {
	podInfo               PodInfo
	vlanProvider          vlanprovider.VlanProvider
	k8sClientSet          kubernetes.Interface
	netAttachDefClientSet clientset.Interface
	netAttachDefsSynced   cache.InformerSynced
	nodesSynced           cache.InformerSynced
	workqueue             workqueue.RateLimitingInterface
}

// NewTopologyController returns new TopologyController instance
func NewTopologyController(
	podInfo PodInfo,
	k8sClientSet kubernetes.Interface,
	netAttachDefClientSet clientset.Interface,
	netAttachDefInformer informers.NetworkAttachmentDefinitionInformer,
	nodeInformer coreInformers.NodeInformer) *TopologyController {

	vlanProvider, err := vlanprovider.NewVlanProvider(podInfo.Provider, podInfo.ProviderConfig)
	if err != nil {
		klog.Fatalf("failed create vlan provider for %s: %s", podInfo.Provider, err.Error())
	}

	TopologyController := &TopologyController{
		podInfo:               podInfo,
		vlanProvider:          vlanProvider,
		k8sClientSet:          k8sClientSet,
		netAttachDefClientSet: netAttachDefClientSet,
		netAttachDefsSynced:   netAttachDefInformer.Informer().HasSynced,
		nodesSynced:           nodeInformer.Informer().HasSynced,
		workqueue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerAgentName),
	}

	netAttachDefInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    TopologyController.handleNetAttachDefAddEvent,
		DeleteFunc: TopologyController.handleNetAttachDefDeleteEvent,
		UpdateFunc: TopologyController.handleNetAttachDefUpdateEvent,
	})

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    TopologyController.handleNodeAddEvent,
		DeleteFunc: TopologyController.handleNodeDeleteEvent,
		UpdateFunc: TopologyController.handleNodeUpdateEvent,
	})

	klog.Infof("Start topocontroller on %s with %T", podInfo.NodeName, vlanProvider)

	return TopologyController
}

func (c *TopologyController) handleNodeAddEvent(obj interface{}) {
	klog.V(3).Info("node add event received")
	node, ok := obj.(*corev1.Node)
	if !ok {
		klog.Error("invalid API object received")
		return
	}
	klog.Infof("Handle NODE CREATE %s", node.ObjectMeta.Name)
	workItem := WorkItem{action: datatypes.NodeAttach, node: node}
	c.workqueue.Add(workItem)
}

func (c *TopologyController) handleNodeDeleteEvent(obj interface{}) {
	klog.V(3).Info("node delete event received")
	node, ok := obj.(*corev1.Node)
	if !ok {
		klog.Error("invalid API object received")
		return
	}
	klog.Infof("Handle NODE DELETE %s", node.ObjectMeta.Name)
	workItem := WorkItem{action: datatypes.NodeDetach, node: node}
	c.workqueue.Add(workItem)
}

func (c *TopologyController) handleNodeUpdateEvent(oldObj, newObj interface{}) {
	klog.V(3).Info("node update event received")
	prev := oldObj.(metav1.Object)
	cur := newObj.(metav1.Object)
	if prev.GetResourceVersion() == cur.GetResourceVersion() {
		return
	}
	oldNode, ok := oldObj.(*corev1.Node)
	if !ok {
		klog.Error("invalid old API object received")
		return
	}
	newNode, ok := newObj.(*corev1.Node)
	if !ok {
		klog.Error("invalid new API object received")
		return
	}
	// Check node annotation change on nokia.com/network-topology, relevant for Openstack only
	anno := newNode.GetAnnotations()
	topo, ok := anno[datatypes.NetworkTopologyKey]
	if ok {
		updated, err := c.vlanProvider.UpdateNodeTopology(newNode.ObjectMeta.Name, topo)
		if err != nil {
			klog.Errorf("Update node topology failed because %s", err.Error())
		} else {
			if updated != topo {
				klog.Infof("Node %s topology updated", newNode.ObjectMeta.Name)
				anno[datatypes.NetworkTopologyKey] = updated
				newNode.SetAnnotations(anno)
				_, err = c.k8sClientSet.CoreV1().Nodes().Update(context.TODO(), newNode, metav1.UpdateOptions{})
				if err != nil {
					klog.Errorf("Update node annotation failed because %s", err.Error())
				}
			}
		}
	}
	// Check node label change
	oldNodeLabels := oldNode.GetLabels()
	newNodeLabels := newNode.GetLabels()
	if reflect.DeepEqual(oldNodeLabels, newNodeLabels) {
		klog.V(3).Info("Node label is not changed")
		return
	}
	klog.Infof("Handle NODE UPDATE %s", newNode.ObjectMeta.Name)
	workItem := WorkItem{action: datatypes.NodeAttachDetach, node: newNode}
	c.workqueue.Add(workItem)
}

func getVlanIds(vlanTrunk string) ([]int, error) {
	result := []int{}
	err := fmt.Errorf("Trunk format is invalid, it should follow this pattern 50,51,700-710")
	if !regexp.MustCompile(`^[0-9\,\-]*$`).MatchString(vlanTrunk) {
		return result, err
	}
	m := strings.Split(vlanTrunk, ",")
	for _, v := range m {
		if strings.Contains(v, "-") {
			n := strings.Split(v, "-")
			if len(n) != 2 {
				return result, err
			}
			min, _ := strconv.Atoi(n[0])
			max, _ := strconv.Atoi(n[1])
			if min == 0 || min > max {
				return result, err
			}
			count := max - min + 1
			for i := 0; i < count; i++ {
				result = append(result, min+i)
			}
		} else {
			vi, _ := strconv.Atoi(v)
			result = append(result, vi)
		}
	}
	return result, nil
}

func (c *TopologyController) shouldTriggerAction(nad *netattachdef.NetworkAttachmentDefinition) (datatypes.NetConf, bool) {
	// Read NAD Config
	var netConf datatypes.NetConf
	err := json.Unmarshal([]byte(nad.Spec.Config), &netConf)
	if err != nil {
		klog.Errorf("read NAD config failed: %s", err.Error())
		return netConf, false
	}
	// Check nodeSelector
	annotationsMap := nad.GetAnnotations()
	ns, ok := annotationsMap[datatypes.NodeSelectorKey]
	if !ok || len(ns) == 0 {
		return netConf, false
	}
	klog.Infof("nodeSelector: %s", ns)
	// Check extProjectID
	project, ok := annotationsMap[datatypes.ExtProjectIDKey]
	if !ok || len(project) == 0 {
		return netConf, false
	}
	klog.Infof("project: %s", project)
	// Check extNetworkID
	network, ok := annotationsMap[datatypes.ExtNetworkIDKey]
	if !ok || len(network) == 0 {
		return netConf, false
	}
	klog.Infof("network: %s", network)
	// Check NAD type
	if netConf.Type == "ipvlan" {
		if netConf.Vlan < 1 || netConf.Vlan > 4095 {
			return netConf, false
		}
		// Check master is tenant-bond.vlan or provider-bond.vlan
		if !strings.HasPrefix(netConf.Master, "tenant-bond.") && !strings.HasPrefix(netConf.Master, "provider-bond.") {
			return netConf, false
		}
		m := strings.Split(netConf.Master, ".")
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return netConf, false
		}
		if v != netConf.Vlan {
			return netConf, false
		}
		return netConf, true
	}
	if netConf.Type == "sriov" {
		if netConf.Vlan == 0 {
			klog.Infof("vlan not present, check vlan_trunk %s", netConf.VlanTrunk)
			if len(netConf.VlanTrunk) == 0 {
				return netConf, false
			}
			_, err = getVlanIds(netConf.VlanTrunk)
			if err != nil {
				klog.Errorf("%s", err.Error())
				return netConf, false
			}

		} else {
			if netConf.Vlan < 1 || netConf.Vlan > 4095 {
				return netConf, false
			}
		}
		resourceName, ok := annotationsMap[datatypes.SriovResourceKey]
		if !ok || len(resourceName) == 0 {
			return netConf, false
		}
		return netConf, true
	}
	return netConf, false
}

func (c *TopologyController) handleNetAttachDefAddEvent(obj interface{}) {
	klog.V(3).Info("net-attach-def add event received")
	nad, ok := obj.(*netattachdef.NetworkAttachmentDefinition)
	if !ok {
		klog.Error("invalid API object received")
		return
	}
	name := nad.ObjectMeta.Name
	namespace := nad.ObjectMeta.Namespace
	klog.Infof("handling nad addition of %s/%s", namespace, name)

	// Check NAD for action
	_, trigger := c.shouldTriggerAction(nad)
	if !trigger {
		klog.Infof("not an action triggering nad, ignored")
		return
	}
	// Handle network attach
	workItem := WorkItem{action: datatypes.CreateAttach, newNad: nad}
	c.workqueue.Add(workItem)
}

func (c *TopologyController) handleNetAttachDefDeleteEvent(obj interface{}) {
	klog.V(3).Info("net-attach-def delete event received")
	nad, ok := obj.(*netattachdef.NetworkAttachmentDefinition)
	if !ok {
		klog.Error("invalid API object received")
		return
	}
	name := nad.ObjectMeta.Name
	namespace := nad.ObjectMeta.Namespace
	klog.Infof("handling nad deletion of %s/%s", namespace, name)

	// Check NAD for action
	_, trigger := c.shouldTriggerAction(nad)
	if !trigger {
		klog.Infof("not an action triggering nad, ignored")
		return
	}
	// Handle network detach
	workItem := WorkItem{action: datatypes.DeleteDetach, newNad: nad}
	c.workqueue.Add(workItem)
}

func (c *TopologyController) handleNetAttachDefUpdateEvent(oldObj, newObj interface{}) {
	klog.V(3).Info("net-attach-def update event received")
	prev := oldObj.(metav1.Object)
	cur := newObj.(metav1.Object)
	if prev.GetResourceVersion() == cur.GetResourceVersion() {
		return
	}
	oldNad, ok := oldObj.(*netattachdef.NetworkAttachmentDefinition)
	if !ok {
		klog.Error("invalid old API object received")
		return
	}
	newNad, ok := newObj.(*netattachdef.NetworkAttachmentDefinition)
	if !ok {
		klog.Error("invalid new API object received")
		return
	}
	name := oldNad.ObjectMeta.Name
	namespace := oldNad.ObjectMeta.Namespace
	klog.Infof("handling nad update of %s/%s", namespace, name)

	// Check NAD for action
	oldNetConf, trigger1 := c.shouldTriggerAction(oldNad)
	newNetConf, trigger2 := c.shouldTriggerAction(newNad)
	klog.Infof("trigger1=%t, trigger2=%t", trigger1, trigger2)
	if !trigger1 && !trigger2 {
		return
	}
	if !trigger1 && trigger2 {
		klog.Infof("NAD is changed to action triggering")
		workItem := WorkItem{action: datatypes.UpdateAttach, newNad: newNad}
		c.workqueue.Add(workItem)
		return
	}
	if trigger1 && !trigger2 {
		klog.Infof("NAD is changed to not action triggering")
		workItem := WorkItem{action: datatypes.UpdateDetach, oldNad: oldNad}
		c.workqueue.Add(workItem)
		return
	}
	// Handle network change
	if oldNetConf.Type != newNetConf.Type {
		klog.Errorf("NAD type change is not supported")
		return
	}
	if oldNetConf.Type == "ipvlan" && oldNetConf.Master != newNetConf.Master {
		klog.Errorf("IPVLAN NAD master device change is not supported")
		return
	}
	if oldNetConf.Vlan > 0 && oldNetConf.Vlan != newNetConf.Vlan {
		klog.Errorf("NAD vlan change is not supported")
		return
	}
	if oldNetConf.Vlan == 0 && oldNetConf.VlanTrunk != newNetConf.VlanTrunk {
		klog.Errorf("SRIOV NAD vlan_trunk change is not supported")
		return
	}
	anno1 := oldNad.GetAnnotations()
	proj1, _ := anno1[datatypes.ExtProjectIDKey]
	net1, _ := anno1[datatypes.ExtNetworkIDKey]
	ns1, _ := anno1[datatypes.NodeSelectorKey]
	anno2 := newNad.GetAnnotations()
	proj2, _ := anno2[datatypes.ExtProjectIDKey]
	net2, _ := anno2[datatypes.ExtNetworkIDKey]
	ns2, _ := anno2[datatypes.NodeSelectorKey]
	if proj1 != proj2 {
		klog.Errorf("NAD project change is not supported")
		return
	}
	if net1 != net2 {
		klog.Errorf("NAD network change is not supported")
		return
	}
	if ns1 == ns2 {
		klog.Info("nodeSelector is not changed")
		return
	}
	workItem := WorkItem{action: datatypes.UpdateAttachDetach, oldNad: oldNad, newNad: newNad}
	c.workqueue.Add(workItem)
}

func (c *TopologyController) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *TopologyController) processNextWorkItem() bool {
	key, shouldQuit := c.workqueue.Get()
	if shouldQuit {
		return false
	}
	defer c.workqueue.Done(key)

	err := c.processItem(key.(WorkItem))
	if err != nil {
		klog.V(4).Infof("work item aborted: %s", err)
	}

	return true
}

func (c *TopologyController) processItem(workItem WorkItem) error {
	switch workItem.action {
	case datatypes.CreateAttach, datatypes.DeleteDetach, datatypes.UpdateAttachDetach, datatypes.UpdateAttach, datatypes.UpdateDetach:
		{
			return c.processNadItem(workItem)
		}
	case datatypes.NodeAttach, datatypes.NodeDetach, datatypes.NodeAttachDetach:
		{
			return c.processNodeItem(workItem)
		}
	}
	return nil
}

// Start runs worker thread after performing cache synchronization
func (c *TopologyController) Start(stopChan <-chan struct{}) {
	klog.V(4).Infof("starting topology controller")

	err := c.vlanProvider.Connect(c.k8sClientSet, c.podInfo.PodNamespace)
	if err != nil {
		klog.Fatalf("Plugin %T failed to connect to remote server %s", c.vlanProvider, err.Error())
	}

	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopChan, c.netAttachDefsSynced, c.nodesSynced); !ok {
		klog.Fatalf("failed waiting for caches to sync")
	}

	go wait.Until(c.worker, time.Second, stopChan)

	<-stopChan
	klog.V(4).Infof("shutting down topology controller")
	return
}
