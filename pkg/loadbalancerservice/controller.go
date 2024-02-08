package loadbalancerservice

import (
	"context"
	"fmt"
	"net"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/cloud-provider/service/helpers"
	"k8s.io/klog/v2"

	"github.com/openshift/microshift/pkg/config"
	"github.com/openshift/microshift/pkg/servicemanager"
)

const defaultInformerResyncPeriod = 30 * time.Second

type LoadbalancerServiceController struct {
	routerIPAddresses []string
	routerHostnames   []string
	routerInterfaces  []string
	NodeIP            string
	KubeConfig        string
	client            *kubernetes.Clientset
	indexer           cache.Indexer
	queue             workqueue.RateLimitingInterface
	informer          cache.SharedIndexInformer
}

var _ servicemanager.Service = &LoadbalancerServiceController{}

func NewLoadbalancerServiceController(cfg *config.Config) *LoadbalancerServiceController {
	return &LoadbalancerServiceController{
		routerIPAddresses: cfg.Ingress.Expose.IPAddresses,
		routerHostnames:   cfg.Ingress.Expose.Hostnames,
		routerInterfaces:  cfg.Ingress.Expose.Interfaces,
		NodeIP:            cfg.Node.NodeIP,
		KubeConfig:        cfg.KubeConfigPath(config.KubeAdmin),
	}
}

func (c *LoadbalancerServiceController) Name() string {
	return "microshift-loadbalancer-service-controller"
}

func (c *LoadbalancerServiceController) Dependencies() []string {
	return []string{
		"network-configuration",
		"kube-apiserver",                  // needed for informers
		"infrastructure-services-manager", // starts CNI
	}
}

func (c *LoadbalancerServiceController) Run(ctx context.Context, ready chan<- struct{}, stopped chan<- struct{}) error {
	defer close(stopped)
	stopCh := make(chan struct{})
	defer close(stopCh)

	restCfg, err := c.restConfig()
	if err != nil {
		return fmt.Errorf("failed to create rest config for service controller: %w", err)
	}
	c.client, err = kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("failed to create clientset for service controller: %w", err)
	}

	klog.Infof("Starting service controller")

	factory := informers.NewSharedInformerFactory(c.client, defaultInformerResyncPeriod)
	serviceInformer := factory.Core().V1().Services()
	c.informer = serviceInformer.Informer()
	c.queue = workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	c.indexer = c.informer.GetIndexer()
	_, err = c.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				c.queue.Add(key)
			}
		},
		UpdateFunc: func(oldObj interface{}, newObj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(newObj)
			if err == nil {
				c.queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				c.queue.Add(key)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("failed to initialize informer event handlers: %w", err)
	}

	factory.Start(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}

	go wait.Until(c.runWorker, time.Second, stopCh)

	close(ready)

	<-ctx.Done()

	return ctx.Err()
}

func (c *LoadbalancerServiceController) runWorker() {
	for c.processNextItem() {
	}
}

func (c *LoadbalancerServiceController) processNextItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.updateServiceStatus(key.(string))
	c.handleErr(err, key)
	return true
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *LoadbalancerServiceController) handleErr(err error, key interface{}) {
	if err == nil {
		c.queue.Forget(key)
		return
	}

	klog.Infof("Error syncing service %v: %v", key, err)

	// Re-enqueue the key rate limited. Based on the rate limiter on the
	// queue and the re-enqueue history, the key will be processed later again.
	c.queue.AddRateLimited(key)
}

func (c *LoadbalancerServiceController) updateServiceStatus(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		klog.Errorf("Fetching service object with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		klog.Infof("Service %s does not exist anymore", key)
	} else {
		svc := obj.(*corev1.Service)
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			return nil
		}
		klog.Infof("Process service %s/%s", svc.Namespace, svc.Name)

		newStatus, err := c.getNewStatus(svc)
		if err != nil {
			return err
		}
		err = c.patchStatus(svc, newStatus)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *LoadbalancerServiceController) restConfig() (*rest.Config, error) {
	return clientcmd.BuildConfigFromFlags("", c.KubeConfig)
}

func (c *LoadbalancerServiceController) getNewStatus(svc *corev1.Service) (*corev1.LoadBalancerStatus, error) {
	newStatus := &corev1.LoadBalancerStatus{}
	objs := c.indexer.List()
	for _, obj := range objs {
		s := obj.(*corev1.Service)
		if (s.Name == svc.Name && s.Namespace == svc.Namespace) || len(s.Status.LoadBalancer.Ingress) == 0 {
			continue
		}
		for _, ep := range s.Spec.Ports {
			for _, np := range svc.Spec.Ports {
				if ep.Port == np.Port {
					klog.Infof("Node port %d occupied", ep.Port)
					return newStatus, fmt.Errorf("node port %d occupied", ep.Port)
				}
			}
		}
	}

	ipList, err := c.getLoadBalancerIngressIPs(svc.Name)
	if err != nil {
		return newStatus, fmt.Errorf("unable to retrieve ip addresses for service: %v", err)
	}
	newStatus.Ingress = make([]corev1.LoadBalancerIngress, len(ipList))
	for i, ip := range ipList {
		newStatus.Ingress[i] = corev1.LoadBalancerIngress{
			IP: ip,
		}
	}

	return newStatus, nil
}

func (c *LoadbalancerServiceController) getLoadBalancerIngressIPs(name string) ([]string, error) {
	ipList := make([]string, 0)
	if name == "router-default" {
		configuredAddresses, err := config.GetConfiguredAddresses()
		if err != nil {
			return ipList, fmt.Errorf("unable to get configured IPs in the host: %v", err)
		}
		for _, ip := range c.routerIPAddresses {
			if slices.Contains(configuredAddresses, ip) {
				ipList = append(ipList, ip)
				continue
			}
			klog.Warningf("router expose IP %v is not configured in the host", ip)
		}
		for _, hostname := range c.routerHostnames {
			ips, err := net.LookupIP(hostname)
			if err != nil {
				klog.Warningf("unable to get IPs for hostname %v: %v", hostname, err)
				continue
			}
			for _, ip := range ips {
				ipList = append(ipList, ip.String())
			}
		}
		for _, nic := range c.routerInterfaces {
			addrs, err := getIpAddressesFromNicName(nic)
			if err != nil {
				klog.Warningf("unable to get IPs for interface %v: %v", nic, err)
				continue
			}
			ipList = append(ipList, addrs...)
		}
	} else {
		ipList = append(ipList, c.NodeIP)
	}
	return ipList, nil
}

func (c *LoadbalancerServiceController) patchStatus(svc *corev1.Service, newStatus *corev1.LoadBalancerStatus) error {
	if helpers.LoadBalancerStatusEqual(&svc.Status.LoadBalancer, newStatus) {
		return nil
	}
	updated := svc.DeepCopy()
	updated.Status.LoadBalancer = *newStatus
	_, err := helpers.PatchService(c.client.CoreV1(), svc, updated)

	return err
}

func getIpAddressesFromNicName(name string) ([]string, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	addrList := make([]string, 0)
	for _, addr := range addrs {
		if ipv4 := addr.(*net.IPNet).IP.To4(); ipv4 != nil {
			addrList = append(addrList, ipv4.String())
		}
	}
	return addrList, nil
}
