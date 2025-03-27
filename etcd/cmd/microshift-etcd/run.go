package main

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/microshift/pkg/config"
	"github.com/openshift/microshift/pkg/util/cryptomaterial"

	"github.com/spf13/cobra"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	etcd "go.etcd.io/etcd/server/v3/embed"
	"go.etcd.io/etcd/server/v3/mvcc/backend"
	"k8s.io/klog/v2"
)

func NewRunEtcdCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "run",
	}
	var multinode bool
	flags := cmd.Flags()
	flags.BoolVar(&multinode, "multinode", false, "enable multinode mode")
	err := flags.MarkHidden("multinode")
	if err != nil {
		panic(err)
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) (err error) {
		cfg, err := config.ActiveConfig()
		if err != nil {
			klog.Fatalf("Error in reading and validating MicroShift config: %v", err)
		}

		e := NewEtcd(cfg, multinode)
		return e.Run()
	}

	return cmd
}

type EtcdService struct {
	etcdCfg                 *etcd.Config
	minDefragBytes          int64
	maxFragmentedPercentage float64
	defragCheckFreq         time.Duration
	multinode               bool
}

func NewEtcd(cfg *config.Config, multinode bool) *EtcdService {
	s := &EtcdService{
		multinode: multinode,
	}
	s.configure(cfg)
	return s
}

func (s *EtcdService) Name() string { return "etcd" }

func (s *EtcdService) configure(cfg *config.Config) {
	s.minDefragBytes = cfg.Etcd.MinDefragBytes
	s.maxFragmentedPercentage = cfg.Etcd.MaxFragmentedPercentage
	s.defragCheckFreq = cfg.Etcd.DefragCheckFreq

	certsDir := cryptomaterial.CertsDirectory(config.DataDir)

	etcdServingCertDir := cryptomaterial.EtcdServingCertDir(certsDir)
	etcdPeerCertDir := cryptomaterial.EtcdPeerCertDir(certsDir)
	etcdSignerCertPath := cryptomaterial.CACertPath(cryptomaterial.EtcdSignerDir(certsDir))
	dataDir := filepath.Join(config.DataDir, s.Name())

	// based on https://github.com/openshift/cluster-etcd-operator/blob/master/bindata/bootkube/bootstrap-manifests/etcd-member-pod.yaml#L19
	s.etcdCfg = etcd.NewConfig()
	s.etcdCfg.ClusterState = "new"
	//s.etcdCfg.ForceNewCluster = true //TODO
	s.etcdCfg.Logger = "zap"
	s.etcdCfg.Dir = dataDir
	s.etcdCfg.QuotaBackendBytes = cfg.Etcd.QuotaBackendBytes
	s.etcdCfg.AdvertisePeerUrls = setURL([]string{cfg.Node.NodeIP}, "2380")
	s.etcdCfg.ListenPeerUrls = setURL([]string{"0.0.0.0"}, "2380")
	s.etcdCfg.AdvertiseClientUrls = setURL([]string{cfg.Node.NodeIP}, "2379")
	s.etcdCfg.ListenClientUrls = setURL([]string{"0.0.0.0"}, "2379")
	// s.etcdCfg.ListenMetricsUrls = setURL([]string{cfg.Node.NodeIP}, "2381")

	s.etcdCfg.Name = cfg.Node.HostnameOverride
	s.etcdCfg.InitialCluster = fmt.Sprintf("%s=https://%s:2380", cfg.Node.HostnameOverride, cfg.Node.NodeIP)

	s.etcdCfg.TlsMinVersion = getTLSMinVersion(cfg.ApiServer.TLS.MinVersion)
	if cfg.ApiServer.TLS.MinVersion != string(configv1.VersionTLS13) {
		s.etcdCfg.CipherSuites = cfg.ApiServer.TLS.CipherSuites
	}
	s.etcdCfg.ClientTLSInfo.CertFile = cryptomaterial.PeerCertPath(etcdServingCertDir)
	s.etcdCfg.ClientTLSInfo.KeyFile = cryptomaterial.PeerKeyPath(etcdServingCertDir)
	s.etcdCfg.ClientTLSInfo.TrustedCAFile = etcdSignerCertPath

	s.etcdCfg.PeerTLSInfo.CertFile = cryptomaterial.PeerCertPath(etcdPeerCertDir)
	s.etcdCfg.PeerTLSInfo.KeyFile = cryptomaterial.PeerKeyPath(etcdPeerCertDir)
	s.etcdCfg.PeerTLSInfo.TrustedCAFile = etcdSignerCertPath

	if s.multinode {
		certsDir := cryptomaterial.CertsDirectory(config.DataDir)
		etcdPeerClientCertDir := cryptomaterial.EtcdPeerCertDir(certsDir)

		klog.Infof("now about to join an existing etcd cluster")
		tlsInfo := transport.TLSInfo{
			CertFile:      cryptomaterial.PeerCertPath(etcdPeerClientCertDir),
			KeyFile:       cryptomaterial.PeerKeyPath(etcdPeerClientCertDir),
			TrustedCAFile: cryptomaterial.CACertPath(cryptomaterial.EtcdSignerDir(certsDir)),
		}
		tlsConfig, err := tlsInfo.ClientConfig()
		if err != nil {
			klog.Errorf("failed to create etcd client TLS config: %v", err)
			return
		}
		klog.Infof("now about to join an existing etcd cluster 2")
		cli, err := clientv3.New(clientv3.Config{
			Endpoints:   []string{"https://microshift-1:2379"},
			DialTimeout: 5 * time.Second,
			TLS:         tlsConfig,
			Context:     context.Background(),
		})
		if err != nil {
			klog.Errorf("failed to create etcd client: %v", err)
			return
		}
		klog.Infof("now about to join an existing etcd cluster 3")
		memberResponse, err := cli.MemberList(context.Background())
		if err != nil {
			klog.Errorf("failed to list etcd members: %v", err)
			return
		}
		klog.Infof("now about to join an existing etcd cluster 4")
		initialCluster := fmt.Sprintf("%s=https://%s:2380", cfg.Node.HostnameOverride, cfg.Node.NodeIP)
		for _, member := range memberResponse.Members {
			if member.Name == cfg.Node.HostnameOverride {
				klog.Infof("etcd member %s already exists", cfg.Node.HostnameOverride)
				continue
			}
			initialCluster = fmt.Sprintf("%s,%s=%s", initialCluster, member.Name, member.PeerURLs[0])
		}
		klog.Infof("initial cluster: %s", initialCluster)
		s.etcdCfg.InitialCluster = initialCluster
		s.etcdCfg.ClusterState = "existing"
		s.etcdCfg.LogLevel = "debug"

		response, err := cli.MemberAdd(context.Background(), []string{fmt.Sprintf("https://%s:2380", cfg.Node.NodeIP)})
		if err != nil {
			klog.Errorf("failed to add etcd member: %v", err)
			return
		}
		klog.Infof("added etcd member %v", response.Member)
	}
}

func (s *EtcdService) Run() error {
	if os.Geteuid() > 0 {
		klog.Fatalf("microshift-etcd must be run privileged")
	}

	versionInfo := EtcdVersionInfo
	klog.InfoS("Version", "microshift-etcd", versionInfo.String(), "etcd-base", versionInfo.EtcdVersion)

	e, err := etcd.StartEtcd(s.etcdCfg)
	if err != nil {
		return fmt.Errorf("microshift-etcd failed to start: %v", err)
	}
	<-e.Server.ReadyNotify()
	defer func() {
		e.Server.Stop()
		<-e.Server.StopNotify()
	}()

	// Go ahead and do a defragment now.
	if err := e.Server.Backend().Defrag(); err != nil {
		err = fmt.Errorf("initial defragmentation failed: %v", err)
		klog.Error(err)
		return err
	}

	// Start up the defrag controller.
	defragCtx, defragShutdown := context.WithCancel(context.Background())
	go s.defragController(defragCtx, e.Server.Backend())

	// Wait to be stopped.
	sigTerm := make(chan os.Signal, 1)
	signal.Notify(sigTerm, os.Interrupt, syscall.SIGTERM)
	sig := <-sigTerm
	klog.Infof("microshift-etcd received signal %v - stopping", sig)

	// Shutdown the defrag controller.
	defragShutdown()

	return nil
}

func (s *EtcdService) defragController(ctx context.Context, be backend.Backend) {
	// Stop the controller if defrags are disabled.
	if s.defragCheckFreq == 0 {
		klog.Warning("defragmentation has been disabled")
		return
	}

	// This timer will check the fragmented conditions periodically.
	timer := time.NewTimer(s.defragCheckFreq)
	defer func() {
		if !timer.Stop() {
			<-timer.C
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case start := <-timer.C:
			if isBackendFragmented(be, s.maxFragmentedPercentage, s.minDefragBytes) {
				klog.Info("attempting to defragment backend")
				if err := be.Defrag(); err != nil {
					klog.Errorf("defragmentation failed: %v", err)
				} else {
					klog.Infof("defragmentation took %v", time.Since(start))
				}
			}
			timer.Reset(s.defragCheckFreq)
		}
	}
}

func setURL(hostnames []string, port string) []url.URL {
	urls := make([]url.URL, len(hostnames))
	for i, name := range hostnames {
		u, err := url.Parse("https://" + net.JoinHostPort(name, port))
		if err != nil {
			klog.Errorf("failed to parse url: %v", err)
			return []url.URL{}
		}
		urls[i] = *u
	}
	return urls
}

func getTLSMinVersion(minVersion string) string {
	switch minVersion {
	case string(configv1.VersionTLS12):
		return "TLS1.2"
	case string(configv1.VersionTLS13):
		return "TLS1.3"
	}
	return ""
}

// The following 'fragemented' logic is copied from the Openshift Cluster Etcd Operator.
//
//	https://github.com/openshift/cluster-etcd-operator/blob/0584b0d1c8868535baf889d8c199f605aef4a3ae/pkg/operator/defragcontroller/defragcontroller.go#L282
func isBackendFragmented(b backend.Backend, maxFragmentedPercentage float64, minDefragBytes int64) bool {
	fragmentedPercentage := checkFragmentationPercentage(b.Size(), b.SizeInUse())
	if fragmentedPercentage > 0.00 {
		klog.Infof("backend store fragmented: %.2f %%, dbSize: %d", fragmentedPercentage, b.Size())
	}
	return fragmentedPercentage >= maxFragmentedPercentage && b.Size() >= minDefragBytes
}

func checkFragmentationPercentage(ondisk, inuse int64) float64 {
	diff := float64(ondisk - inuse)
	fragmentedPercentage := (diff / float64(ondisk)) * 100
	return math.Round(fragmentedPercentage*100) / 100
}
