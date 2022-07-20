package topocontroller

import (
	"context"
	"reflect"
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
	klog.Infof("Received NODE CREATE %s", node.ObjectMeta.Name)
	// Event is handled in NODE UPDATE upon node toplogy is annotated by the node agent
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
	// Check node annotation change on nokia.com/network-topology
	anno1 := oldNode.GetAnnotations()
	topo1, _ := anno1[datatypes.NetworkTopologyKey]
	anno := newNode.GetAnnotations()
	topo, ok := anno[datatypes.NetworkTopologyKey]
	if !ok {
		return
	}
	if topo1 != topo {
		// No-op for BM
		updated, err := c.vlanProvider.UpdateNodeTopology(newNode.ObjectMeta.Name, topo)
		if err != nil {
			klog.Errorf("Update node topology failed because %s", err.Error())
			return
		}
		if updated != topo {
			klog.Infof("Node %s topology updated", newNode.ObjectMeta.Name)
			anno[datatypes.NetworkTopologyKey] = updated
			newNode.SetAnnotations(anno)
			_, err = c.k8sClientSet.CoreV1().Nodes().Update(context.TODO(), newNode, metav1.UpdateOptions{})
			if err != nil {
				klog.Errorf("Update node annotation failed because %s", err.Error())
				return
			}
		}
		klog.Infof("Handle NODE CREATE %s", newNode.ObjectMeta.Name)
		workItem := WorkItem{action: datatypes.NodeAttach, node: newNode}
		c.workqueue.Add(workItem)
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
	_, trigger, err := datatypes.ShouldTriggerTopoAction(nad)
	if err != nil || !trigger {
		klog.Infof("%s/%s is not an action triggering nad: ignored", namespace, name)
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
	_, trigger, err := datatypes.ShouldTriggerTopoAction(nad)
	if err != nil || !trigger {
		klog.Infof("%s/%s is not an action triggering nad: ignored", namespace, name)
		return
	}
	// Handle network detach
	workItem := WorkItem{action: datatypes.DeleteDetach, oldNad: nad, newNad: nad}
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
	updateAction, err := datatypes.ShouldTriggerTopoUpdate(oldNad, newNad)
	if err != nil || updateAction == 0 {
		klog.Infof("%s/%s does not have an action triggering update: ignored", namespace, name)
		return
	}
	workItem := WorkItem{action: updateAction, oldNad: oldNad, newNad: newNad}
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
