/*
Copyright © 2025 MicroShift Contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/openshift/microshift/pkg/config"
	"github.com/openshift/microshift/pkg/telemetry"
	"k8s.io/klog/v2"
	_ "k8s.io/klog/v2"
)

type TelemetryManager struct {
	config *config.Config
}

func NewTelemetryManager(cfg *config.Config) *TelemetryManager {
	return &TelemetryManager{
		config: cfg,
	}
}

func (t *TelemetryManager) Name() string { return "telemetry-manager" }
func (t *TelemetryManager) Dependencies() []string {
	return []string{"kube-apiserver", "cluster-id-manager"}
}

func (t *TelemetryManager) Run(ctx context.Context, ready chan<- struct{}, stopped chan<- struct{}) error {
	defer close(stopped)
	defer close(ready)
	clusterId, err := getClusterId()
	if err != nil {
		return fmt.Errorf("unable to get cluster id: %v", err)
	}
	pullSecret, err := readPullSecret("/etc/crio/openshift-pull-secret")
	if err != nil {
		return fmt.Errorf("unable to get pull secret: %v", err)
	}
	telemeter := telemetry.NewTelemetry(clusterId, pullSecret)
	go func() {
		err := telemeter.Send(ctx, nil)
		if err != nil {
			klog.Infof("unable to send metrics: %v", err)
		} else {
			klog.Infof("metrics sent successfully")
		}
	}()
	return ctx.Err()
}

func getClusterId() (string, error) {
	filePath := "/var/lib/microshift/cluster-id"
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %v", err)
	}
	return string(data), nil
}

func readPullSecret(filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %v", err)
	}
	var jsonData map[string]interface{}
	err = json.Unmarshal(data, &jsonData)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal JSON: %v", err)
	}
	auths, ok := jsonData["auths"]
	if !ok {
		return "", fmt.Errorf("auths not found")
	}
	cloudOpenshiftCom, ok := auths.(map[string]interface{})["cloud.openshift.com"]
	if !ok {
		return "", fmt.Errorf("cloud.openshift.com not found")
	}
	auth, ok := cloudOpenshiftCom.(map[string]interface{})["auth"]
	if !ok {
		return "", fmt.Errorf("auth not found")
	}
	return auth.(string), nil
}
