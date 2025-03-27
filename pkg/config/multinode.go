package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/openshift/microshift/pkg/util/cryptomaterial"
	"k8s.io/klog/v2"
)

type MultiNodeConfig struct {
	Enabled bool `json:"-"`
	Join    bool `json:"-"`
	Kubelet bool `json:"-"`
	// only one controlplane node is supported
	// IP address of control plane node
	Controlplane string `json:"controlplane"`
	// this is the bootstrap kubeconfig to join nodes to the cluster
	KubeConfig string `json:"kubeconfig"`

	EtcdCACert string
	EtcdCAKey  string
}

// ConfigMultiNode populates multinode configurations to Config.MultiNode
func ConfigMultiNode(c *Config, enabled bool, bootstrapKubeConfig, etcdCACert, etcdCAKey string) *Config {
	c.MultiNode = MultiNodeConfig{
		Enabled:      false,
		Join:         false,
		Kubelet:      false,
		Controlplane: c.Node.NodeIP,
		KubeConfig:   bootstrapKubeConfig,
		EtcdCACert:   etcdCACert,
		EtcdCAKey:    etcdCAKey,
	}
	if !enabled && bootstrapKubeConfig == "" {
		return c
	}
	c.MultiNode.Enabled = true
	if bootstrapKubeConfig != "" && etcdCACert == "" && etcdCAKey == "" {
		c.MultiNode.Kubelet = true
	}
	if c.MultiNode.KubeConfig != "" {
		c.MultiNode.Join = true
	}
	// Use controlplane node IP as APIServer backend (instead of next available
	// IP from service network)
	c.ApiServer.AdvertiseAddress = c.Node.NodeIP
	c.ApiServer.AdvertiseAddresses = []string{c.Node.NodeIP}
	c.ApiServer.SkipInterface = true

	klog.Infof("multinode configuration: %v", c.MultiNode)
	return c
}

func UpdateMultiNode(c *Config) {
	if c.MultiNode.Join {
		if c.MultiNode.EtcdCACert != "" && c.MultiNode.EtcdCAKey != "" {
			caCert, caKey, err := loadCA(c.MultiNode.EtcdCACert, c.MultiNode.EtcdCAKey)
			if err != nil {
				klog.Errorf("Error loading CA: %v", err)
				return
			}

			certsDir := cryptomaterial.CertsDirectory(DataDir)
			etcdServingCertDir := cryptomaterial.EtcdServingCertDir(certsDir)
			etcdPeerCertDir := cryptomaterial.EtcdPeerCertDir(certsDir)

			copyFile(c.MultiNode.EtcdCACert, cryptomaterial.CACertPath(cryptomaterial.EtcdSignerDir(certsDir)))
			copyFile(c.MultiNode.EtcdCAKey, cryptomaterial.CAKeyPath(cryptomaterial.EtcdSignerDir(certsDir)))

			certConfigs := []CertConfig{
				{
					CommonName:  "system:etcd-peer:etcd-client",
					OrgName:     "system:etcd-peers",
					DNSNames:    []string{"localhost", c.Node.HostnameOverride},
					IPAddresses: []string{"127.0.0.1", "::1", c.Node.NodeIP},
					CertPath:    cryptomaterial.PeerCertPath(etcdPeerCertDir),
					KeyPath:     cryptomaterial.PeerKeyPath(etcdPeerCertDir),
				},
				{
					CommonName:  "system:etcd-server:etcd-client",
					OrgName:     "system:etcd-servers",
					DNSNames:    []string{"localhost", c.Node.HostnameOverride},
					IPAddresses: []string{"127.0.0.1", "::1", c.Node.NodeIP},
					CertPath:    cryptomaterial.PeerCertPath(etcdServingCertDir),
					KeyPath:     cryptomaterial.PeerKeyPath(etcdServingCertDir),
				},
			}

			// Generate each certificate
			for _, config := range certConfigs {
				err := generateCertificate(caCert, caKey, config)
				if err != nil {
					klog.Errorf("Error generating certificate for %s: %v", config.CommonName, err)
				}
			}
		}
	}
}

// Certificate configuration struct
type CertConfig struct {
	CommonName  string
	OrgName     string
	DNSNames    []string
	IPAddresses []string
	CertPath    string
	KeyPath     string
}

// Load CA certificate and private key from files
func loadCA(certPath, keyPath string) (*x509.Certificate, interface{}, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}

	// Parse CA certificate
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, nil, fmt.Errorf("failed to parse CA certificate")
	}
	caCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}

	// Parse CA private key
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("failed to parse CA private key")
	}
	caKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}

	return caCert, caKey, nil
}

// Generate a new certificate signed by the CA
func generateCertificate(caCert *x509.Certificate, caKey interface{}, config CertConfig) error {
	// Generate a new private key for the certificate
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	// Convert IP string slices to net.IP
	var ipAddresses []net.IP
	for _, ip := range config.IPAddresses {
		parsedIP := net.ParseIP(ip)
		if parsedIP != nil {
			ipAddresses = append(ipAddresses, parsedIP)
		}
	}

	// Define certificate template
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}

	pubKeyBytes, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		return err
	}
	ski := sha1.Sum(pubKeyBytes)

	certTemplate := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   config.CommonName,
			Organization: []string{config.OrgName},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              config.DNSNames,
		IPAddresses:           ipAddresses,
		SubjectKeyId:          ski[:],
	}

	// Create certificate signed by CA
	certBytes, err := x509.CreateCertificate(rand.Reader, &certTemplate, caCert, &privKey.PublicKey, caKey)
	if err != nil {
		return err
	}

	// Save certificate
	certFile, err := os.Create(config.CertPath)
	if err != nil {
		return err
	}
	defer certFile.Close()
	err = pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certBytes})
	if err != nil {
		return err
	}

	// Save private key
	keyFile, err := os.Create(config.KeyPath)
	if err != nil {
		return err
	}
	defer keyFile.Close()
	err = pem.Encode(keyFile, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privKey)})
	if err != nil {
		return err
	}

	fmt.Printf("Generated certificate: %s\n", config.CertPath)
	fmt.Printf("Generated key: %s\n", config.KeyPath)
	return nil
}

func copyFile(source, destination string) error {
	input, err := os.ReadFile(source)
	if err != nil {
		klog.Errorf("Failed to read source file %s: %v", source, err)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), os.FileMode(0700)); err != nil {
		klog.Errorf("Failed to create directory for destination file %s: %v", destination, err)
		return err
	}
	if err := os.WriteFile(destination, input, 0600); err != nil {
		klog.Errorf("Failed to write to destination file %s: %v", destination, err)
		return err
	}
	return nil
}
