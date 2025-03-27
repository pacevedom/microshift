package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"

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
		//TODO temporary. working this out.
		caCert, caKey, err := loadCA("/home/microshift/ca.crt", "/home/microshift/ca.key")
		if err != nil {
			klog.Errorf("Error loading CA: %v", err)
			return c
		}
		// Define two different certificate configurations
		certConfigs := []CertConfig{
			{
				CommonName:  "system:etcd-peer:etcd-client",
				OrgName:     "system:etcd-peers",
				DNSNames:    []string{"localhost", "microshift-2"},
				IPAddresses: []string{},
				CertPath:    "/home/microshift/etcd-peer.crt",
				KeyPath:     "/home/microshift/etcd-peer.key",
			},
			{
				CommonName:  "system:etcd-server:etcd-client",
				OrgName:     "system:etcd-servers",
				DNSNames:    []string{"localhost", "microshift-2"},
				IPAddresses: []string{},
				CertPath:    "/home/microshift/etcd-serving.crt",
				KeyPath:     "/home/microshift/etcd-serving.key",
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
	klog.Infof("multinode configuration: %v", c.MultiNode)
	return c
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

	certTemplate := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   config.CommonName,
			Organization: []string{config.OrgName},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour), // 1 year validity
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              config.DNSNames,
		IPAddresses:           ipAddresses,
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
