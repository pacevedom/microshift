package cmd

import (
	"context"
	"crypto/x509"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	librarycrypto "github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/microshift/pkg/config"
	"github.com/openshift/microshift/pkg/util/cryptomaterial"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/authentication/user"

	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const (
	// Default kubeconfig location for joining cluster
	defaultJoinKubeConfig = "/tmp/microshift-join-kubeconfig"
	// Default timeout for operations
	joinDefaultTimeout = 10 * time.Minute
	// Secret name for etcd CA certificate (from addnode.go)
	joinEtcdCASecretName = "microshift-etcd-ca"
	// Secret namespace (from addnode.go)
	joinEtcdCASecretNamespace = "kube-system"
)

type JoinClusterOptions struct {
	KubeconfigPath string
	Timeout        time.Duration
	Learner        bool
}

func NewJoinClusterCommand() *cobra.Command {
	opts := &JoinClusterOptions{
		KubeconfigPath: defaultJoinKubeConfig,
		Timeout:        joinDefaultTimeout,
	}

	cmd := &cobra.Command{
		Use:    "join-cluster",
		Short:  "Join a node to an existing MicroShift cluster",
		Hidden: true,
		Long: `This command joins a node to an existing MicroShift cluster by:
1. Loading the MicroShift configuration from files
2. Fetching etcd CA certificate and key from the cluster using provided kubeconfig
3. Generating etcd certificates (serving, peer, client) using the CA
4. Configuring etcd to join the cluster as learner or member based on node count
5. Configuring kubelet with the certificates
6. Restarting the MicroShift systemd unit
7. Verifying the node is ready in the cluster`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJoinCluster(cmd.Context(), opts)
		},
	}

	cmd.Flags().StringVar(&opts.KubeconfigPath, "kubeconfig", opts.KubeconfigPath,
		"Path to kubeconfig file for connecting to the cluster")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", opts.Timeout,
		"Timeout for cluster join operations")
	cmd.Flags().BoolVar(&opts.Learner, "learner", false,
		"Join the cluster as a learner node (default is to join as a member)")

	return cmd
}

func runJoinCluster(ctx context.Context, opts *JoinClusterOptions) error {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	klog.Info("Starting cluster join process...")
	if opts.Learner {
		klog.Info("Will add etcd node as learner")
	}

	cfg, err := config.ActiveConfig()
	if err != nil {
		return fmt.Errorf("failed to load MicroShift configuration: %w", err)
	}
	klog.Info("MicroShift configuration loaded successfully")

	client, err := createKubernetesClient(opts.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	caCert, caKey, serial, err := fetchEtcdCA(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to fetch etcd CA: %w", err)
	}
	klog.Info("Etcd CA certificate and key retrieved successfully")

	if err := generateEtcdCertificates(cfg, caCert, caKey, serial); err != nil {
		return fmt.Errorf("failed to generate etcd certificates: %w", err)
	}
	klog.Info("Etcd certificates generated successfully")

	_, clusterMembers, err := getClusterInfo(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to get cluster information: %w", err)
	}

	if err := configureEtcdForCluster(cfg, clusterMembers, opts.Learner); err != nil {
		return fmt.Errorf("failed to configure etcd for cluster: %w", err)
	}

	if err := configureBootstrapKubeconfig(cfg, opts.KubeconfigPath); err != nil {
		return fmt.Errorf("failed to configure bootstrap kubeconfig: %w", err)
	}

	if err := restartMicroShift(); err != nil {
		return fmt.Errorf("failed to restart MicroShift service: %w", err)
	}
	klog.Info("MicroShift service restarted")

	if err := waitForNodeReady(ctx, client, cfg.CanonicalNodeName()); err != nil {
		return fmt.Errorf("node failed to become ready: %w", err)
	}

	klog.Info("Node successfully joined the cluster and is ready!")
	return nil
}

func createKubernetesClient(kubeconfigPath string) (kubernetes.Interface, error) {
	if _, err := os.Stat(kubeconfigPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("kubeconfig file does not exist at %s", kubeconfigPath)
	}

	_, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("invalid kubeconfig file: %w", err)
	}

	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(restConfig)
}

func fetchEtcdCA(ctx context.Context, client kubernetes.Interface) ([]byte, []byte, []byte, error) {
	secret, err := client.CoreV1().Secrets(joinEtcdCASecretNamespace).Get(ctx, joinEtcdCASecretName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get etcd CA secret: %w", err)
	}

	caCert, exists := secret.Data["ca.crt"]
	if !exists {
		return nil, nil, nil, fmt.Errorf("ca.crt not found in secret")
	}

	caKey, exists := secret.Data["ca.key"]
	if !exists {
		return nil, nil, nil, fmt.Errorf("ca.key not found in secret")
	}

	serial, exists := secret.Data["serial.txt"]
	if !exists {
		return nil, nil, nil, fmt.Errorf("serial.txt not found in secret")
	}

	return caCert, caKey, serial, nil
}

func generateEtcdCertificates(cfg *config.Config, caCertPEM, caKeyPEM, serial []byte) error {
	certsDir := cryptomaterial.CertsDirectory(config.DataDir)
	etcdSignerDir := cryptomaterial.EtcdSignerDir(certsDir)

	// Ensure directories exist
	if err := os.MkdirAll(etcdSignerDir, 0755); err != nil {
		return fmt.Errorf("failed to create etcd signer directory: %w", err)
	}

	// Write CA certificate and key
	caCertPath := cryptomaterial.CACertPath(etcdSignerDir)
	caKeyPath := cryptomaterial.CAKeyPath(etcdSignerDir)

	if err := os.WriteFile(caCertPath, caCertPEM, 0644); err != nil {
		return fmt.Errorf("failed to write CA certificate: %w", err)
	}

	if err := os.WriteFile(caKeyPath, caKeyPEM, 0600); err != nil {
		return fmt.Errorf("failed to write CA key: %w", err)
	}

	if err := os.WriteFile(filepath.Join(etcdSignerDir, "serial.txt"), serial, 0644); err != nil {
		return fmt.Errorf("failed to write CA serial: %w", err)
	}

	// Create CA config from the provided cert and key
	caTLSConfig, err := librarycrypto.GetTLSCertificateConfigFromBytes(caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("failed to load CA certificate config: %w", err)
	}

	// Create CA object for signing
	caConfig := &librarycrypto.CA{
		Config:          caTLSConfig,
		SerialGenerator: &librarycrypto.RandomSerialGenerator{},
	}

	// Create directories for etcd certificates
	servingCertDir := cryptomaterial.EtcdServingCertDir(certsDir)
	if err := os.MkdirAll(servingCertDir, 0755); err != nil {
		return fmt.Errorf("failed to create serving cert directory: %w", err)
	}

	peerCertDir := cryptomaterial.EtcdPeerCertDir(certsDir)
	if err := os.MkdirAll(peerCertDir, 0755); err != nil {
		return fmt.Errorf("failed to create peer cert directory: %w", err)
	}

	clientCertDir := cryptomaterial.EtcdAPIServerClientCertDir(certsDir)
	if err := os.MkdirAll(clientCertDir, 0755); err != nil {
		return fmt.Errorf("failed to create client cert directory: %w", err)
	}

	// Prepare hostnames and IPs for etcd certificates
	hostnames := []string{"localhost", cfg.Node.HostnameOverride}
	ips := []net.IP{net.ParseIP("127.0.0.1")}
	if cfg.Node.NodeIP != "" {
		if ip := net.ParseIP(cfg.Node.NodeIP); ip != nil {
			ips = append(ips, ip)
		}
	}

	//TODO something is wrong with serial numbers. investigate.

	// Generate serving certificate
	servingTLS, err := caConfig.MakeServerCertForDuration(
		sets.New[string](hostnames...),
		time.Duration(cryptomaterial.LongLivedCertificateValidityDays)*24*time.Hour,
		func(certTemplate *x509.Certificate) error {
			certTemplate.Subject.CommonName = "system:etcd-server:etcd-client"
			certTemplate.Subject.Organization = []string{"system:etcd-servers"}
			certTemplate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
			certTemplate.IPAddresses = ips
			certTemplate.SerialNumber = big.NewInt(4)
			serialNumberPath := filepath.Join(servingCertDir, "serial.txt")
			if err := os.WriteFile(serialNumberPath, []byte(certTemplate.SerialNumber.String()), 0644); err != nil {
				return fmt.Errorf("failed to write serial number to disk: %w", err)
			}
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("failed to generate serving certificate: %w", err)
	}

	servingCertPath := cryptomaterial.PeerCertPath(servingCertDir)
	servingKeyPath := cryptomaterial.PeerKeyPath(servingCertDir)
	if err := servingTLS.WriteCertConfigFile(servingCertPath, servingKeyPath); err != nil {
		return fmt.Errorf("failed to write serving certificate: %w", err)
	}

	peerTLS, err := caConfig.MakeServerCertForDuration(
		sets.New[string](hostnames...),
		time.Duration(cryptomaterial.LongLivedCertificateValidityDays)*24*time.Hour,
		func(certTemplate *x509.Certificate) error {
			certTemplate.Subject.CommonName = "system:etcd-peer:etcd-client"
			certTemplate.Subject.Organization = []string{"system:etcd-peers"}
			certTemplate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
			certTemplate.IPAddresses = ips
			certTemplate.SerialNumber = big.NewInt(4)
			serialNumberPath := filepath.Join(peerCertDir, "serial.txt")
			if err := os.WriteFile(serialNumberPath, []byte(certTemplate.SerialNumber.String()), 0644); err != nil {
				return fmt.Errorf("failed to write serial number to disk: %w", err)
			}
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("failed to generate peer certificate: %w", err)
	}

	peerCertPath := cryptomaterial.PeerCertPath(peerCertDir)
	peerKeyPath := cryptomaterial.PeerKeyPath(peerCertDir)
	if err := peerTLS.WriteCertConfigFile(peerCertPath, peerKeyPath); err != nil {
		return fmt.Errorf("failed to write peer certificate: %w", err)
	}

	// Generate client certificate
	clientUserInfo := &user.DefaultInfo{Name: "etcd", Groups: []string{"etcd"}}
	clientTLS, err := caConfig.MakeClientCertificateForDuration(
		clientUserInfo,
		time.Duration(cryptomaterial.LongLivedCertificateValidityDays)*24*time.Hour,
	)
	if err != nil {
		return fmt.Errorf("failed to generate client certificate: %w", err)
	}

	clientCertPath := cryptomaterial.ClientCertPath(clientCertDir)
	clientKeyPath := cryptomaterial.ClientKeyPath(clientCertDir)
	if err := clientTLS.WriteCertConfigFile(clientCertPath, clientKeyPath); err != nil {
		return fmt.Errorf("failed to write client certificate: %w", err)
	}

	return nil
}

func getClusterInfo(ctx context.Context, client kubernetes.Interface) (int, []string, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	readyCount := 0
	var members []string
	for _, node := range nodes.Items {
		if isJoinNodeReady(&node) {
			readyCount++
			nodeIP := ""
			for _, addr := range node.Status.Addresses {
				if addr.Type == corev1.NodeInternalIP {
					nodeIP = addr.Address
					break
				}
			}
			if nodeIP != "" {
				members = append(members, fmt.Sprintf("%s=https://%s:2380", node.Name, nodeIP))
			}
		}
	}

	return readyCount, members, nil
}

func isJoinNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func configureEtcdForCluster(cfg *config.Config, clusterMembers []string, isLearner bool) error {
	// Create etcd configuration for joining cluster
	dataDir := filepath.Join(config.DataDir, "etcd")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create etcd data directory: %w", err)
	}

	// Add current node to the cluster members list
	nodeIP := cfg.Node.NodeIP
	if nodeIP == "" {
		nodeIP = "127.0.0.1" // fallback
	}
	currentNodeMember := fmt.Sprintf("%s=https://%s:2380", cfg.CanonicalNodeName(), nodeIP)
	cfgInitialCluster := append(clusterMembers, currentNodeMember)

	clusterConfig := fmt.Sprintf("etcd:\n  initialCluster: %s\n  clusterState: existing", strings.Join(cfgInitialCluster, ","))

	configDir := "/etc/microshift/config.d"
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create configuration directory: %w", err)
	}

	configFilePath := filepath.Join(configDir, "99-etcd.yaml")
	if err := os.WriteFile(configFilePath, []byte(clusterConfig), 0644); err != nil {
		return fmt.Errorf("failed to write etcd cluster configuration: %w", err)
	}

	certsDir := cryptomaterial.CertsDirectory(config.DataDir)
	etcdPeerClientCertDir := cryptomaterial.EtcdPeerCertDir(certsDir)

	tlsInfo := transport.TLSInfo{
		CertFile:      cryptomaterial.PeerCertPath(etcdPeerClientCertDir),
		KeyFile:       cryptomaterial.PeerKeyPath(etcdPeerClientCertDir),
		TrustedCAFile: cryptomaterial.CACertPath(cryptomaterial.EtcdSignerDir(certsDir)),
	}
	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		return fmt.Errorf("failed to create etcd client TLS config: %v", err)
	}

	var endpoints []string
	for _, member := range clusterMembers {
		parts := strings.SplitN(member, "=", 2)
		if len(parts) == 2 {
			endpoint := strings.Replace(parts[1], ":2380", ":2379", 1)
			endpoints = append(endpoints, endpoint)
		}
	}
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
		TLS:         tlsConfig,
		Context:     context.Background(),
	})
	if err != nil {
		return fmt.Errorf("failed to create etcd client: %v", err)
	}

	memberResponse, err := client.MemberList(context.Background())
	if err != nil {
		return fmt.Errorf("failed to list etcd members: %v", err)
	}

	var filteredEndpoints []string
	initialCluster := fmt.Sprintf("%s=https://%s:2380", cfg.Node.HostnameOverride, cfg.Node.NodeIP)
	for _, member := range memberResponse.Members {
		if member.Name == cfg.Node.HostnameOverride {
			continue
		}
		if !member.IsLearner {
			filteredEndpoints = append(filteredEndpoints, member.ClientURLs[0])
		}
		initialCluster = fmt.Sprintf("%s,%s=%s", initialCluster, member.Name, member.PeerURLs[0])
	}

	client, err = clientv3.New(clientv3.Config{
		Endpoints:   filteredEndpoints,
		DialTimeout: 5 * time.Second,
		TLS:         tlsConfig,
		Context:     context.Background(),
	})
	if err != nil {
		return fmt.Errorf("failed to create etcd client with filtered endpoints: %v", err)
	}

	addFunction := client.MemberAdd
	if isLearner {
		addFunction = client.MemberAddAsLearner
	}
	response, err := addFunction(context.Background(), []string{fmt.Sprintf("https://%s:2380", cfg.Node.NodeIP)})
	if err != nil {
		return fmt.Errorf("failed to add etcd node: %v", err)
	}
	klog.Infof("Successfully added etcd node: %v", response)
	return nil
}

func configureBootstrapKubeconfig(cfg *config.Config, kubeconfigPath string) error {
	bootstrapKubeConfigPath := cfg.BootstrapKubeConfigPath()
	if err := os.MkdirAll(filepath.Dir(bootstrapKubeConfigPath), 0755); err != nil {
		return fmt.Errorf("failed to create kubelet directory: %w", err)
	}

	if err := copyFile(kubeconfigPath, bootstrapKubeConfigPath); err != nil {
		return fmt.Errorf("failed to copy kubeconfig for kubelet: %w", err)
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}

func restartMicroShift() error {
	cmd := exec.Command("systemctl", "restart", "microshift")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to restart microshift service: %w", err)
	}
	return nil
}

func waitForNodeReady(ctx context.Context, client kubernetes.Interface, nodeName string) error {
	klog.Infof("Waiting for node %s to become ready...", nodeName)

	timeout := time.After(5 * time.Minute)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for node to become ready")
		case <-ticker.C:
			node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				klog.Warningf("Failed to get node %s: %v", nodeName, err)
				continue
			}

			if isJoinNodeReady(node) {
				klog.Infof("Node %s is ready!", nodeName)
				return nil
			}

			klog.Infof("Node %s is not ready yet, waiting...", nodeName)
		}
	}
}
