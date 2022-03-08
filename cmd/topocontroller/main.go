package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreSharedInformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog"

	clientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	sharedInformers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions"

	"github.com/nokia/net-attach-def-admission-controller/pkg/topocontroller"
)

var (
	// defines default resync period between k8s API server and controller
	syncPeriod = time.Second * 600
)

func main() {
	var (
		provider, providerConfig string
		nodeName                 = os.Getenv("NODE_NAME")
		nodeNamespace            = os.Getenv("POD_NAMESPACE")
	)

	klog.InitFlags(nil)
	flag.StringVar(&provider, "provider", "baremetal", "Only baremetal and openstack are supported.")
	flag.StringVar(&providerConfig, "provider-config", "/etc/config/fss.conf", "File containing credentials to access external provider")
	flag.Parse()

	cfg, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("error building kubeconfig: %s", err.Error())
	}

	k8sClientSet, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("error creating kubernetes clientset: %s", err.Error())
	}

	netAttachDefClientSet, err := clientset.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("error creating net-attach-def clientset: %s", err.Error())
	}

	netAttachDefInformerFactory := sharedInformers.NewSharedInformerFactory(netAttachDefClientSet, syncPeriod)
	k8sInformerFactory := coreSharedInformers.NewSharedInformerFactory(k8sClientSet, syncPeriod)

	topologyController := topocontroller.NewTopologyController(
		provider,
		nodeName,
		providerConfig,
		k8sClientSet,
		netAttachDefClientSet,
		netAttachDefInformerFactory.K8sCniCncfIo().V1().NetworkAttachmentDefinitions(),
		k8sInformerFactory.Core().V1().Nodes(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
	go func() {
		<-c
		cancel()
		os.Exit(1)
	}()

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      "topocontroller",
			Namespace: nodeNamespace,
		},
		Client: k8sClientSet.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: nodeName,
		},
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   60 * time.Second,
		RenewDeadline:   15 * time.Second,
		RetryPeriod:     5 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				klog.Info("start leading")
				netAttachDefInformerFactory.Start(ctx.Done())
				k8sInformerFactory.Start(ctx.Done())
				topologyController.Start(ctx.Done())
			},
			OnStoppedLeading: func() {
				klog.Info("stopped leading")
			},
			OnNewLeader: func(identity string) {
				if identity == nodeName {
					klog.Info("obtained leadership")
					return
				}
				klog.Infof("leader elected: %s", identity)
			},
		},
	})
}
