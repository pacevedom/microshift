package config

import "k8s.io/klog/v2"

type MultiNodeConfig struct {
	Enabled bool `json:"-"`
	Join    bool `json:"-"`
	Kubelet bool `json:"-"`
	// only one controlplane node is supported
	// IP address of control plane node
	Controlplane string `json:"controlplane"`
	// this is the bootstrap kubeconfig to join nodes to the cluster
	KubeConfig string `json:"kubeconfig"`
}

// ConfigMultiNode populates multinode configurations to Config.MultiNode
func ConfigMultiNode(c *Config, enabled, kubeletOnly bool, bootstrapKubeConfig string) *Config {
	c.MultiNode = MultiNodeConfig{
		Enabled:      false,
		Join:         false,
		Kubelet:      false,
		Controlplane: "",
		KubeConfig:   "",
	}
	if !enabled && bootstrapKubeConfig == "" {
		return c
	}
	c.MultiNode.Enabled = true
	c.MultiNode.Kubelet = kubeletOnly
	c.MultiNode.Controlplane = c.Node.NodeIP
	// Use controlplane node IP as APIServer backend (instead of next available
	// IP from service network)
	c.ApiServer.AdvertiseAddress = c.Node.NodeIP
	c.ApiServer.AdvertiseAddresses = []string{c.Node.NodeIP}
	// Don't configure the advertise address on the device.
	c.ApiServer.SkipInterface = true
	c.MultiNode.KubeConfig = bootstrapKubeConfig
	if c.MultiNode.KubeConfig != "" {
		c.MultiNode.Join = true
	}
	klog.Infof("multinode configuration: %v", c.MultiNode)
	return c
}
