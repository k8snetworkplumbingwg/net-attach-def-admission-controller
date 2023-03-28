package netcontroller

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
)

const (
	controllerAgentName = "net-attach-def-netcontroller"
)

type NodeInfo struct {
	Provider   string
	NodeName   string
	NodeLabels map[string]string
	VlanMap    map[string][]string
}

type WorkItem struct {
	action     datatypes.NadAction
	nad        *netattachdef.NetworkAttachmentDefinition
	vlanIfName string
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
	node.Annotations[datatypes.NetworkTopologyKey] = string(topology)
	node, err = k8sClientSet.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
	if err != nil {
		klog.Fatalf("error updating node annonation: %s", err.Error())
	}

	nodeInfo := NodeInfo{
		Provider:   provider,
		NodeName:   nodeName,
		NodeLabels: node.Labels,
		VlanMap:    make(map[string][]string),
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
		klog.V(3).Infof("%s=%s", k, v)
	}
	klog.Infof("topology=%s", topology)

	return NetworkController
}

func (c *NetworkController) shouldTriggerAction(nad *netattachdef.NetworkAttachmentDefinition) (datatypes.NetConf, bool) {
	// Get NAD Config
	netConf, err := datatypes.GetNetConf(nad)
	if err != nil {
		return netConf, false
	}
	// Check NAD type
	if netConf.Type != "ipvlan" {
		return netConf, false
	}
	if netConf.Vlan < 1 || netConf.Vlan > 4095 {
		return netConf, false
	}
	// Check master is tenant.vlan or provider.vlan
	if !strings.HasPrefix(netConf.Master, "tenant.") && !strings.HasPrefix(netConf.Master, "provider.") {
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
	ns, ok := annotationsMap[datatypes.NodeSelectorKey]
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
		klog.Infof("NAD ADD of %s/%s is not an action triggering nad: ignored", namespace, name)
		return
	}

	// Create vlan interface
	workItem := WorkItem{action: datatypes.Create, nad: nad, vlanIfName: netConf.Master}
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
		klog.Infof("NAD UPDATE of %s/%s is not an action triggering nad: ignored", namespace, name)
		return
	}

	// Check if node becomes eligible
	if !trigger1 && trigger2 {
		workItem := WorkItem{action: datatypes.Create, nad: newNad, vlanIfName: newNetConf.Master}
		c.workqueue.Add(workItem)
	}

	// Check if node becomes not eligible
	if trigger1 && !trigger2 {
		workItem := WorkItem{action: datatypes.Delete, nad: oldNad, vlanIfName: oldNetConf.Master}
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
		klog.Infof("NAD DELETE of %s/%s is not an action triggering nad: ignored", namespace, name)
		return
	}

	// Delete vlan interface
	workItem := WorkItem{action: datatypes.Delete, nad: nad, vlanIfName: netConf.Master}
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
	klog.Infof("handling node %s label change", c.nodeInfo.NodeName)
	c.nodeInfo.NodeLabels = newNodeLabels
	nadList, err := c.netAttachDefClientSet.K8sCniCncfIoV1().NetworkAttachmentDefinitions("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		klog.Errorf("List network attachment definitions failed because %s", err.Error())
		return
	}
	for _, nad := range nadList.Items {
		name := nad.ObjectMeta.Name
		namespace := nad.ObjectMeta.Namespace
		klog.Infof("Checking NAD %s/%s", namespace, name)
		// Check NAD for action
		netConf, trigger := c.shouldTriggerAction(&nad)
		if !trigger {
			// Delete vlan interface if exists
			if getVlanInterface(netConf.Master) {
				workItem := WorkItem{action: datatypes.Delete, nad: &nad, vlanIfName: netConf.Master}
				c.workqueue.Add(workItem)
			}
		} else {
			// Create vlan interface if not exists
			if !getVlanInterface(netConf.Master) {
				workItem := WorkItem{action: datatypes.Create, nad: &nad, vlanIfName: netConf.Master}
				c.workqueue.Add(workItem)
			}
		}
	}
}

func (c *NetworkController) updateNadAnnotations(nad *netattachdef.NetworkAttachmentDefinition, status string) error {
	for i := 0; i < 256; i++ {
		nad, err := c.netAttachDefClientSet.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.ObjectMeta.Namespace).Get(context.TODO(), nad.ObjectMeta.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		anno := nad.GetAnnotations()
		if status == "deleted" {
			_, ok := anno[c.nodeInfo.NodeName]
			if !ok {
				return nil
			}
			delete(anno, c.nodeInfo.NodeName)
		} else {
			anno[c.nodeInfo.NodeName] = status
		}
		nad.SetAnnotations(anno)
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
	case datatypes.Create:
		{
			klog.Infof("Create vlan interface of %s", workItem.vlanIfName)
			nadName := fmt.Sprintf("%s/%s", workItem.nad.ObjectMeta.Namespace, workItem.nad.ObjectMeta.Name)
			numUsers, err := createVlanInterface(c.nodeInfo.VlanMap, nadName, workItem.vlanIfName)
			if err == nil {
				if numUsers == 0 {
					klog.Infof("vlan interface %s is created", workItem.vlanIfName)
				} else {
					klog.Infof("vlan interface %s now has %d users", workItem.vlanIfName, numUsers)
				}
				c.updateNadAnnotations(workItem.nad, "created")
			} else {
				klog.Errorf("vlan interface %s creation failed: %s", workItem.vlanIfName, err.Error())
				c.updateNadAnnotations(workItem.nad, "creation-failed")
			}
			return err
		}
	case datatypes.Delete:
		{
			klog.Infof("Delete vlan interface of %s", workItem.vlanIfName)
			nadName := fmt.Sprintf("%s/%s", workItem.nad.ObjectMeta.Namespace, workItem.nad.ObjectMeta.Name)
			numUsers, err := deleteVlanInterface(c.nodeInfo.VlanMap, nadName, workItem.vlanIfName)
			if err != nil {
				klog.Errorf("vlan interface %s deletion failed: %s", workItem.vlanIfName, err.Error())
				c.updateNadAnnotations(workItem.nad, "deletion-failed")
			}
			if numUsers == 0 {
				klog.Infof("vlan interface %s is deleted", workItem.vlanIfName)
			} else {
				klog.Infof("vlan interface %s now has %d users", workItem.vlanIfName, numUsers)
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
