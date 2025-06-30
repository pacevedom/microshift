package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/openshift/microshift/pkg/config"
	"github.com/openshift/microshift/pkg/util/cryptomaterial"
	"github.com/spf13/cobra"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const (
	// Default timeout for waiting for new node to join
	defaultNodeJoinTimeout = 10 * time.Minute
	// Secret name for etcd CA certificate
	etcdCASecretName = "microshift-etcd-ca"
	// Secret namespace
	etcdCASecretNamespace = "kube-system"
)

func NewAddNodeMicroshiftCommand() *cobra.Command {
	var nodeJoinTimeout time.Duration

	cmd := &cobra.Command{
		Use:    "add-node",
		Short:  "Add a node to MicroShift cluster by exposing etcd CA and waiting for node to join",
		Hidden: true,
		Long: `This command prepares the cluster to accept a new node by:
1. Expose the etcd CA certificate in a Kubernetes secret
2. Wait for a new node to join the cluster
3. Cleaning up resources after successful join or timeout`,
	}

	cmd.Flags().DurationVar(&nodeJoinTimeout, "timeout", defaultNodeJoinTimeout, "Timeout for waiting for new node to join")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return runAddNode(cmd.Context(), nodeJoinTimeout)
	}

	return cmd
}

func runAddNode(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		klog.Info("Received interrupt signal, cancelling...")
		cancel()
	}()

	ctx, timeoutCancel := context.WithTimeout(ctx, timeout)
	defer timeoutCancel()

	client, err := getKubernetesClient()
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	klog.Info("Exposing etcd CA certificate...")
	if err := exposeEtcdCA(ctx, client); err != nil {
		return fmt.Errorf("failed to expose etcd CA: %w", err)
	} else {
		klog.Infof("Successfully created etcd CA secret %s/%s", etcdCASecretNamespace, etcdCASecretName)
	}
	defer func() {
		klog.Info("Cleaning up etcd CA secret...")
		err := cleanupEtcdCA(context.Background(), client)
		if err != nil {
			klog.Errorf("Failed to cleanup etcd CA secret: %v", err)
		} else {
			klog.Infof("Successfully cleaned up etcd CA secret %s/%s", etcdCASecretNamespace, etcdCASecretName)
		}
	}()

	initialNodes, err := getNodeCount(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to get initial node count: %w", err)
	}
	klog.Infof("Current number of nodes: %d", initialNodes)

	klog.Infof("Waiting %v for a new node to join the cluster...", timeout)
	newNodeName, err := waitForNewNode(ctx, client, initialNodes)
	if err != nil {
		return fmt.Errorf("failed to wait for new node: %w", err)
	}

	klog.Infof("Successfully detected new node: %s", newNodeName)
	klog.Info("Add node operation completed successfully!")
	return nil
}

func getKubernetesClient() (kubernetes.Interface, error) {
	cfg := config.NewDefault()
	kubeconfig := cfg.KubeConfigPath(config.KubeAdmin)

	var restConfig *rest.Config
	var err error

	if kubeconfig != "" {
		restConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		restConfig, err = rest.InClusterConfig()
	}

	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(restConfig)
}

func exposeEtcdCA(ctx context.Context, client kubernetes.Interface) error {
	certsDir := cryptomaterial.CertsDirectory(config.DataDir)
	etcdSignerDir := cryptomaterial.EtcdSignerDir(certsDir)
	etcdCACertPath := cryptomaterial.CACertPath(etcdSignerDir)
	etcdCAKeyPath := cryptomaterial.CAKeyPath(etcdSignerDir)

	caCert, err := os.ReadFile(etcdCACertPath)
	if err != nil {
		return fmt.Errorf("failed to read etcd CA certificate from %s: %w", etcdCACertPath, err)
	}

	caKey, err := os.ReadFile(etcdCAKeyPath)
	if err != nil {
		return fmt.Errorf("failed to read etcd CA key from %s: %w", etcdCAKeyPath, err)
	}

	serial, err := os.ReadFile(filepath.Join(etcdSignerDir, "serial.txt"))
	if err != nil {
		return fmt.Errorf("failed to read CA serial: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      etcdCASecretName,
			Namespace: etcdCASecretNamespace,
			Labels: map[string]string{
				"app":       "microshift",
				"component": "etcd-ca",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.crt":     caCert,
			"ca.key":     caKey,
			"serial.txt": serial,
		},
	}

	_, err = client.CoreV1().Secrets(etcdCASecretNamespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create etcd CA secret: %w", err)
	}
	return nil
}

func getNodeCount(ctx context.Context, client kubernetes.Interface) (int, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}

	return len(nodes.Items), nil
}

func waitForNewNode(ctx context.Context, client kubernetes.Interface, initialNodeCount int) (string, error) {
	nodesList, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to list nodes: %w", err)
	}

	watchInterface, err := client.CoreV1().Nodes().Watch(ctx, metav1.ListOptions{
		Watch:           true,
		ResourceVersion: nodesList.ResourceVersion,
	})
	if err != nil {
		return "", fmt.Errorf("failed to start watching nodes: %w", err)
	}
	defer watchInterface.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timeout or cancellation while waiting for new node")
		case event, ok := <-watchInterface.ResultChan():
			if !ok {
				return "", fmt.Errorf("watch channel closed unexpectedly")
			}

			if event.Type == watch.Added {
				if node, ok := event.Object.(*corev1.Node); ok {
					currentCount, err := getNodeCount(ctx, client)
					if err != nil {
						klog.Errorf("Failed to get current node count: %v", err)
						continue
					}

					if currentCount > initialNodeCount {
						return node.Name, nil
					}
				}
			}
		}
	}
}

func cleanupEtcdCA(ctx context.Context, client kubernetes.Interface) error {
	err := client.CoreV1().Secrets(etcdCASecretNamespace).Delete(ctx, etcdCASecretName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete etcd CA secret: %w", err)
	}
	return nil
}
