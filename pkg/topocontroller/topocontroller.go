package topocontroller

import (
	"context"
	"encoding/json"
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

	"github.com/nokia/net-attach-def-admission-controller/pkg/vlanprovider"
)

const (
	nodeSelectorKey     = "k8s.v1.cni.cncf.io/nodeSelector"
	extNetworkIDKey     = "nokia.com/extNetworkID"
	networkTopologyKey  = "nokia.com/network-topology"
	controllerAgentName = "net-attach-def-topocontroller"
)

type NetConf struct {
	types.NetConf
	Master string `json:"master,omitempty"`
	Vlan   int    `json:"vlan,omitempty"`
}

type NadAction int

const (
	//Attach ... open vlan on switch
	Attach NadAction = 1
	//UpdateNodes ... nodes using vlan changed
	UpdateNodes NadAction = 2
	//Detach ... close vlan on switch
	Detach NadAction = 3
)

type WorkItem struct {
	action NadAction
	oldNad *netattachdef.NetworkAttachmentDefinition
	newNad *netattachdef.NetworkAttachmentDefinition
}

// TopologyController is the controller implementation for FSS Operator
type TopologyController struct {
	vlanProvider          vlanprovider.VlanProvider
	k8sClientSet          kubernetes.Interface
	netAttachDefClientSet clientset.Interface
	netAttachDefsSynced   cache.InformerSynced
	nodesSynced           cache.InformerSynced
	workqueue             workqueue.RateLimitingInterface
}

// NewTopologyController returns new TopologyController instance
func NewTopologyController(
	provider string,
	providerConfig string,
	k8sClientSet kubernetes.Interface,
	netAttachDefClientSet clientset.Interface,
	netAttachDefInformer informers.NetworkAttachmentDefinitionInformer,
	nodeInformer coreInformers.NodeInformer) *TopologyController {

	vlanProvider, err := vlanprovider.NewVlanProvider(provider, providerConfig)
	if err != nil {
		klog.Fatalf("failed create vlan provider for %s: %s", provider, err.Error())
	}

	TopologyController := &TopologyController{
		vlanProvider:          vlanProvider,
		k8sClientSet:          k8sClientSet,
		netAttachDefClientSet: netAttachDefClientSet,
		netAttachDefsSynced:   netAttachDefInformer.Informer().HasSynced,
		nodesSynced:           nodeInformer.Informer().HasSynced,
		workqueue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerAgentName),
	}

	netAttachDefInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    TopologyController.handleNetAttachDefAddEvent,
		UpdateFunc: TopologyController.handleNetAttachDefUpdateEvent,
		DeleteFunc: TopologyController.handleNetAttachDefDeleteEvent,
	})

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: TopologyController.handleNodeUpdateEvent,
	})

	klog.Infof("Start topocontroller for %T", vlanProvider)

	return TopologyController
}

func (c *TopologyController) handleNodeUpdateEvent(oldObj, newObj interface{}) {
	klog.V(3).Info("node update event received")
	prev := oldObj.(metav1.Object)
	cur := newObj.(metav1.Object)
	if prev.GetResourceVersion() == cur.GetResourceVersion() {
		return
	}
	_, ok := oldObj.(*corev1.Node)
	if !ok {
		klog.Error("invalid old API object received")
		return
	}
	newNode, ok := newObj.(*corev1.Node)
	if !ok {
		klog.Error("invalid new API object received")
		return
	}
	anno := newNode.GetAnnotations()
	topo, ok := anno[networkTopologyKey]
	if !ok {
		return
	}
	updated, err := c.vlanProvider.UpdateNodeTopology(newNode.ObjectMeta.Name, topo)
	if err != nil {
		klog.Errorf("Update node topology skipped because %s", err.Error())
		return
	}
	if updated != topo {
		klog.Infof("Node %s topology=%s", newNode.ObjectMeta.Name, updated)
		anno[networkTopologyKey] = updated
		newNode.SetAnnotations(anno)
		_, err = c.k8sClientSet.CoreV1().Nodes().Update(context.TODO(), newNode, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("Update node annotaton failed because %s", err.Error())
			return
		}
	}
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
	klog.Infof("handling addition of %s/%s", namespace, name)

	// Check NAD for action
	netConf, trigger := c.shouldTriggerAction(nad)
	if !trigger {
		klog.Infof("not an action triggering nad, ignored")
		return
	}
	// Handle network attach
	klog.Infof("Attach vlan interface of %s", netConf.Master)
	workItem := WorkItem{action: Attach, newNad: nad}
	c.workqueue.Add(workItem)
}

func (c *TopologyController) shouldTriggerAction(nad *netattachdef.NetworkAttachmentDefinition) (NetConf, bool) {
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
	// Check extNetworkID
	networks, ok := annotationsMap[extNetworkIDKey]
	if !ok || len(ns) == 0 {
		return netConf, false
	}
	klog.Infof("networks: %s", networks)
	return netConf, true
}

func (c *TopologyController) GetNodes(nad *netattachdef.NetworkAttachmentDefinition) []string {
	annotationsMap := nad.GetAnnotations()
	ns, _ := annotationsMap[nodeSelectorKey]
	nodes, err := c.k8sClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: ns})
	if err != nil {
		klog.Error("Get matching nodes failed: %s", err.Error())
		return nil
	}
	var names []string
	for _, node := range nodes.Items {
		names = append(names, node.ObjectMeta.Name)
	}
	return names
}

func (c *TopologyController) GetChangedNodes(oldNad, newNad *netattachdef.NetworkAttachmentDefinition) ([]string, []string) {
	oldNodes := c.GetNodes(oldNad)
	newNodes := c.GetNodes(newNad)
	if len(oldNodes) == 0 || len(newNodes) == 0 {
		return oldNodes, newNodes
	}
	m := make(map[string]int)
	for _, node := range oldNodes {
		m[node] = 1
	}
	for _, node := range newNodes {
		m[node] = m[node] + 1
	}
	// Get changed nodes
	var changedNodes []string
	for k, v := range m {
		if v == 1 {
			changedNodes = append(changedNodes, k)
		}
	}
	var nodesToDetach []string
	var nodesToAttach []string
	for _, c := range changedNodes {
		for _, node := range oldNodes {
			if c == node {
				nodesToDetach = append(nodesToDetach, c)
			}
		}
		for _, node := range newNodes {
			if c == node {
				nodesToAttach = append(nodesToAttach, c)
			}
		}
	}
	return nodesToDetach, nodesToAttach
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
	klog.Infof("handling update of %s/%s", namespace, name)

	// Check NAD for action
	oldNetConf, trigger1 := c.shouldTriggerAction(oldNad)
	newNetConf, trigger2 := c.shouldTriggerAction(newNad)
	if !trigger1 && !trigger2 {
		return
	}
	// should not happen, give explicit error
	if !trigger1 && trigger2 {
		klog.Error("should not happen: NAD changed to action triggering, ignored")
		return
	}
	// should not happen, give explicit error
	if trigger1 && !trigger2 {
		klog.Error("should not happen: NAD changed to not action triggering, ignored")
		return
	}
	// should not happen, give explicit error
	if oldNetConf.Master != newNetConf.Master {
		klog.Error("should not happen: master device changed from %s to %s, ignored", oldNetConf.Master, newNetConf.Master)
		return
	}
	// Handle network change
	klog.Infof("Update nodes using vlan interface of %s", newNetConf.Master)
	workItem := WorkItem{action: UpdateNodes, oldNad: oldNad, newNad: newNad}
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
	klog.Infof("handling deletion of %s/%s", namespace, name)

	// Check NAD for action
	netConf, trigger := c.shouldTriggerAction(nad)
	if !trigger {
		klog.Infof("not an action triggering nad, ignored")
		return
	}
	// Handle network detach
	klog.Infof("Detach vlan interface of %s", netConf.Master)
	workItem := WorkItem{action: Detach, newNad: nad}
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
	var netConf NetConf
	json.Unmarshal([]byte(workItem.newNad.Spec.Config), &netConf)
	annotationsMap := workItem.newNad.GetAnnotations()
	networks, _ := annotationsMap[extNetworkIDKey]
	switch workItem.action {
	case Attach:
		{
			nodes := c.GetNodes(workItem.newNad)
			return c.vlanProvider.Attach(networks, netConf.Vlan, nodes)
		}
	case UpdateNodes:
		{
			nodesToDetach, nodesToAttach := c.GetChangedNodes(workItem.oldNad, workItem.newNad)
			c.vlanProvider.Detach("my-network", netConf.Vlan, nodesToDetach)
			return c.vlanProvider.Attach(networks, netConf.Vlan, nodesToAttach)
		}
	case Detach:
		{
			nodes := c.GetNodes(workItem.newNad)
			return c.vlanProvider.Detach(networks, netConf.Vlan, nodes)
		}
	}
	return nil
}

// Start runs worker thread after performing cache synchronization
func (c *TopologyController) Start(stopChan <-chan struct{}) {
	klog.V(4).Infof("starting topology controller")
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopChan, c.netAttachDefsSynced, c.nodesSynced); !ok {
		klog.Fatalf("failed waiting for caches to sync")
	}

	go wait.Until(c.worker, time.Second, stopChan)

	<-stopChan
	klog.V(4).Infof("shutting down topology controller")
	return
}
