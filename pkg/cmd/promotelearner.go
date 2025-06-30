package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openshift/microshift/pkg/config"
	"github.com/openshift/microshift/pkg/util/cryptomaterial"
	"github.com/spf13/cobra"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"

	"k8s.io/klog/v2"
)

func NewPromoteLearnerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "promote-learner",
		Short:  "Promote a learner node in the etcd cluster",
		Hidden: true,
		Long: `Promote a learner node to a voting member in the etcd cluster.
		This command promotes any learner node found in the etcd cluster, as there can
		only be one learner at a time.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPromoteLearner(cmd.Context())
		},
	}
	return cmd
}

func runPromoteLearner(ctx context.Context) error {
	klog.Info("Starting learner promotion process")

	cfg, err := config.ActiveConfig()
	if err != nil {
		return fmt.Errorf("failed to load MicroShift configuration: %w", err)
	}

	klog.Infof("Creating Kubernetes client from %s", cfg.KubeConfigPath(config.KubeAdmin))
	client, err := createKubernetesClient(cfg.KubeConfigPath(config.KubeAdmin))
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	klog.Info("Retrieving cluster information")
	_, clusterMembers, err := getClusterInfo(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to get cluster information: %w", err)
	}

	klog.Info("Promoting etcd learners to members")
	if err := promoteEtcdLearners(clusterMembers, cfg); err != nil {
		return fmt.Errorf("failed to promote etcd learners: %w", err)
	}

	klog.Info("Restarting MicroShift service")
	if err := restartMicroShift(); err != nil {
		return fmt.Errorf("failed to restart MicroShift service: %w", err)
	}
	klog.Info("Learner promotion process completed successfully")
	return nil
}

func promoteEtcdLearners(clusterMembers []string, cfg *config.Config) error {
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
	for _, member := range memberResponse.Members {
		if !member.IsLearner {
			filteredEndpoints = append(filteredEndpoints, member.ClientURLs...)
		}
	}
	klog.Infof("filtered endpoints: %v", filteredEndpoints)

	// Rebuild the etcd client with filtered endpoints
	client, err = clientv3.New(clientv3.Config{
		Endpoints:   filteredEndpoints,
		DialTimeout: 5 * time.Second,
		TLS:         tlsConfig,
		Context:     context.Background(),
	})
	if err != nil {
		return fmt.Errorf("failed to create etcd client with filtered endpoints: %v", err)
	}
	defer client.Close()
	members, err := client.MemberList(context.Background())
	if err != nil {
		return fmt.Errorf("failed to list etcd members: %v", err)
	}
	for _, member := range members.Members {
		if member.IsLearner {
			klog.Infof("Promoting %s", member.Name)
			response, err := client.MemberPromote(context.Background(), member.ID)
			if err != nil {
				return fmt.Errorf("failed to promote etcd learner %s: %v", member.Name, err)
			}
			klog.Infof("Successfully promoted etcd learner %s with response: %v", member.Name, response)
		}
	}
	return nil
}
