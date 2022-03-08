package netcontroller

import (
	"context"
	"encoding/json"
	"reflect"
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
	"gopkg.in/intel/multus-cni.v3/pkg/types"
)

const (
	nodeSelectorKey     = "k8s.v1.cni.cncf.io/nodeSelector"
	networkTopologyKey  = "nokia.com/network-topology"
	controllerAgentName = "net-attach-def-netcontroller"
)

type NodeInfo struct {
	Provider   string
	NodeName   string
	NodeLabels map[string]string
}

type NetConf struct {
	types.NetConf
	Master string `json:"master,omitempty"`
	Vlan   int    `json:"vlan,omitempty"`
}

type NadAction int

const (
	//Create ... create vlan interface
	Create NadAction = 1
	//Detach ... delete vlan interface
	Delete NadAction = 2
)

type WorkItem struct {
	action     NadAction
	nad        *netattachdef.NetworkAttachmentDefinition
	vlanIfName string
	vlanId     int
}

// NetworkController is the controller implementation for VLAN Operator
type NetworkController struct {
	nodeInfo              NodeInfo
	k8sClientSet          kubernetes.Interface
	netAttachDefClientSet clientset.Interface
	netAttachDefsSynced   cache.InformerSynced
	nodesSynced           cache.InformerSynced
	workqueue             workqueue.RateLimitingInterface
}

// NewNetworkController returns new NetworkController instance
func NewNetworkController(
	provider string,
	nodeName string,
	k8sClientSet kubernetes.Interface,
	netAttachDefClientSet clientset.Interface,
	netAttachDefInformer informers.NetworkAttachmentDefinitionInformer,
	nodeInformer coreInformers.NodeInformer) *NetworkController {

	// Get node labels
	node, err := k8sClientSet.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		klog.Fatalf("error getting node info: %s", err.Error())
	}

	// Get node topology and set it in node annotations
	topology, err := getNodeTopology(provider)
	if err != nil {
		klog.Fatalf("error getting node topology: %s", err.Error())
	}
	node.Annotations[networkTopologyKey] = string(topology)
	node, err = k8sClientSet.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
	if err != nil {
		klog.Fatalf("error updating node annonation: %s", err.Error())
	}

	nodeInfo := NodeInfo{
		Provider:   provider,
		NodeName:   nodeName,
		NodeLabels: node.Labels,
	}

	NetworkController := &NetworkController{
		nodeInfo:              nodeInfo,
		k8sClientSet:          k8sClientSet,
		netAttachDefClientSet: netAttachDefClientSet,
		netAttachDefsSynced:   netAttachDefInformer.Informer().HasSynced,
		nodesSynced:           nodeInformer.Informer().HasSynced,
		workqueue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerAgentName),
	}

	netAttachDefInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    NetworkController.handleNetAttachDefAddEvent,
		UpdateFunc: NetworkController.handleNetAttachDefUpdateEvent,
		DeleteFunc: NetworkController.handleNetAttachDefDeleteEvent,
	})

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: NetworkController.handleNodeUpdateEvent,
	})

	klog.Infof("Start netcontroller on %s for %s", nodeInfo.NodeName, nodeInfo.Provider)
	for k, v := range nodeInfo.NodeLabels {
		klog.Infof("%s=%s", k, v)
	}
	klog.Infof("topology=%s", topology)

	return NetworkController
}

func (c *NetworkController) shouldTriggerAction(nad *netattachdef.NetworkAttachmentDefinition) (NetConf, bool) {
	// Read NAD Config
	var netConf NetConf
	err := json.Unmarshal([]byte(nad.Spec.Config), &netConf)
	if err != nil {
		klog.Error("read NAD config failed: %s", err.Error())
		return netConf, false
	}
	// Check NAD type
	if netConf.Type != "ipvlan" {
		return netConf, false
	}
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
	// Check nodeSelector
	annotationsMap := nad.GetAnnotations()
	ns, ok := annotationsMap[nodeSelectorKey]
	if !ok || len(ns) == 0 {
		return netConf, false
	}
	klog.Infof("nodeSelector: %s", ns)
	label := strings.Split(ns, "=")
	if len(label) == 2 {
		if v, ok := c.nodeInfo.NodeLabels[label[0]]; ok {
			if v == label[1] {
				klog.Infof("the node has the label: %s=%s", label[0], v)
				return netConf, true
			}
		}
	}
	return netConf, false
}

func (c *NetworkController) handleNetAttachDefAddEvent(obj interface{}) {
	klog.V(3).Info("net-attach-def add event received")
	nad, ok := obj.(*netattachdef.NetworkAttachmentDefinition)
	if !ok {
		klog.Error("invalid API object received")
		return
	}
	name := nad.ObjectMeta.Name
	namespace := nad.ObjectMeta.Namespace
	klog.Infof("handling addition of %s/%s", namespace, name)

	// Check NAD for action
	netConf, trigger := c.shouldTriggerAction(nad)
	if !trigger {
		klog.Infof("not an action triggering nad, ignored")
		return
	}

	// Create vlan interface
	workItem := WorkItem{action: Create, nad: nad, vlanIfName: netConf.Master, vlanId: netConf.Vlan}
	c.workqueue.Add(workItem)
}

func (c *NetworkController) handleNetAttachDefUpdateEvent(oldObj, newObj interface{}) {
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
	klog.Infof("handling update of %s/%s", namespace, name)

	// Check NAD for action
	oldNetConf, trigger1 := c.shouldTriggerAction(oldNad)
	newNetConf, trigger2 := c.shouldTriggerAction(newNad)
	if trigger1 == trigger2 {
		klog.Infof("not an action triggering update, ignored")
		return
	}

	// Check if node becomes eligible
	if !trigger1 && trigger2 {
		workItem := WorkItem{action: Create, nad: newNad, vlanIfName: newNetConf.Master, vlanId: newNetConf.Vlan}
		c.workqueue.Add(workItem)
	}

	// Check if node becomes not eligible
	if trigger1 && !trigger2 {
		workItem := WorkItem{action: Delete, nad: oldNad, vlanIfName: oldNetConf.Master}
		c.workqueue.Add(workItem)
	}
}

func (c *NetworkController) handleNetAttachDefDeleteEvent(obj interface{}) {
	klog.V(3).Info("net-attach-def delete event received")
	nad, ok := obj.(*netattachdef.NetworkAttachmentDefinition)
	if !ok {
		klog.Error("invalid API object received")
		return
	}
	name := nad.ObjectMeta.Name
	namespace := nad.ObjectMeta.Namespace
	klog.Infof("handling deletion of %s/%s", namespace, name)

	// Check NAD for action
	netConf, trigger := c.shouldTriggerAction(nad)
	if !trigger {
		klog.Infof("not an action triggering nad, ignored")
		return
	}

	// Delete vlan interface
	workItem := WorkItem{action: Delete, nad: nad, vlanIfName: netConf.Master}
	c.workqueue.Add(workItem)
}

func (c *NetworkController) handleNodeUpdateEvent(oldObj, newObj interface{}) {
	klog.V(3).Info("node update event received")
	prev := oldObj.(metav1.Object)
	cur := newObj.(metav1.Object)
	if prev.GetResourceVersion() == cur.GetResourceVersion() {
		return
	}
	oldNode, ok := oldObj.(*corev1.Node)
	if !ok {
		klog.Error("invalid new API object received")
		return
	}
	if oldNode.GetName() != c.nodeInfo.NodeName {
		return
	}
	newNode, ok := newObj.(*corev1.Node)
	if !ok {
		klog.Error("invalid new API object received")
		return
	}
	if newNode.GetName() != c.nodeInfo.NodeName {
		return
	}
	newNodeLabels := newNode.GetLabels()
	if reflect.DeepEqual(c.nodeInfo.NodeLabels, newNodeLabels) {
		return
	}
	c.nodeInfo.NodeLabels = newNodeLabels
	list, err := c.netAttachDefClientSet.K8sCniCncfIoV1().NetworkAttachmentDefinitions("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		klog.Errorf("List network attachment definitions failed because %s", err.Error())
	}
	for _, nad := range list.Items {
		name := nad.ObjectMeta.Name
		namespace := nad.ObjectMeta.Namespace
		klog.Infof("Checking NAD %s/%s", namespace, name)
		// Check NAD for action
		netConf, trigger := c.shouldTriggerAction(&nad)
		if !trigger {
			// Delete vlan interface if exists
			if getVlanInterface(netConf.Master) {
				workItem := WorkItem{action: Delete, nad: &nad, vlanIfName: netConf.Master}
				c.workqueue.Add(workItem)
			}
		} else {
			// Create vlan interface if not exists
			if !getVlanInterface(netConf.Master) {
				workItem := WorkItem{action: Create, nad: &nad, vlanIfName: netConf.Master, vlanId: netConf.Vlan}
				c.workqueue.Add(workItem)
			}
		}
	}
}

func (c *NetworkController) updateNadAnnotations(nad *netattachdef.NetworkAttachmentDefinition, status string) {
	anno := nad.GetAnnotations()
	if status == "deleted" {
		_, ok := anno[c.nodeInfo.NodeName]
		if !ok {
			return
		}
		delete(anno, c.nodeInfo.NodeName)
	} else {
		anno[c.nodeInfo.NodeName] = status
	}
	nad.SetAnnotations(anno)
	_, err := c.netAttachDefClientSet.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.ObjectMeta.Namespace).Update(context.TODO(), nad, metav1.UpdateOptions{})
	if err != nil {
		klog.Errorf("Update NAD annotaton failed because %s", err.Error())
		return
	}
}

func (c *NetworkController) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *NetworkController) processNextWorkItem() bool {
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

func (c *NetworkController) processItem(workItem WorkItem) error {
	switch workItem.action {
	case Create:
		{
			klog.Infof("Create vlan interface of %s", workItem.vlanIfName)
			err := createVlanInterface(workItem.vlanIfName, workItem.vlanId)
			if err != nil {
				klog.Errorf("vlan interface is not created because %s", err.Error())
				c.updateNadAnnotations(workItem.nad, "creation-failed")
			}
			return err
		}
	case Delete:
		{
			klog.Infof("Delete vlan interface of %s", workItem.vlanIfName)
			err := deleteVlanInterface(workItem.vlanIfName)
			if err != nil {
				klog.Errorf("vlan interface deletion failed because %s", err.Error())
			}
			c.updateNadAnnotations(workItem.nad, "deleted")
			return err
		}
	}
	return nil
}

// Start runs worker thread after performing cache synchronization
func (c *NetworkController) Start(stopChan <-chan struct{}) {
	klog.V(4).Infof("starting network controller")
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopChan, c.netAttachDefsSynced, c.nodesSynced); !ok {
		klog.Fatalf("failed waiting for caches to sync")
	}

	go wait.Until(c.worker, time.Second, stopChan)

	<-stopChan
	klog.V(4).Infof("shutting down network controller")
	return
}
