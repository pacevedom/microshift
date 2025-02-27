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
package telemetry

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/openshift/microshift/pkg/config"
	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/prometheus/prometheus/prompb"
	"k8s.io/klog/v2"
)

const (
	authString = `{"authorization_token": "%s", "cluster_id": "%s"}`
)

type Metric struct {
	Name      string        `json:"name"`
	Labels    []MetricLabel `json:"labels"`
	Timestamp int64         `json:"timestamp"`
	Value     float64       `json:"value"`
}

type MetricLabel struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Telemetry struct {
	encodedAuth string
	endpoint    string
	clusterId   string
	//TODO aqui necesito mas datos. commit id, cluster id
	// tambien necesito la metrica de uso de cpu que lei antes.
}

func NewTelemetry(baseURL, clusterId, pullSecret string) *Telemetry {
	authString := fmt.Sprintf(authString, pullSecret, clusterId)
	encodedAuth := base64.StdEncoding.EncodeToString([]byte(authString))
	return &Telemetry{
		encodedAuth: encodedAuth,
		endpoint:    fmt.Sprintf("%s/metrics/v1/receive", baseURL),
		clusterId:   clusterId,
	}
}

func (t *Telemetry) Send(ctx context.Context, metrics []Metric) error {
	wr := convertMetricsToWriteRequest(metrics)
	data, err := proto.Marshal(wr)
	if err != nil {
		return fmt.Errorf("failed to marshal WriteRequest: %v", err)
	}
	compressed := snappy.Encode(nil, data)
	reader := bytes.NewReader(compressed)

	req, err := http.NewRequestWithContext(ctx, "POST", t.endpoint, reader)
	if err != nil {
		return fmt.Errorf("unable to create request: %v", err)
	}

	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", t.encodedAuth))

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("unable to do the request: %v", err)
	}
	defer func() {
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			klog.Error(err, "error discarding body")
		}
		resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("unable to read body: %v", err)
	}
	return fmt.Errorf("request not successful. Status code %v. Body %v", resp.StatusCode, string(body))
}

func (t *Telemetry) CollectMetrics(cfg *config.Config) ([]Metric, error) {
	kubeletMetrics, err := fetchKubeletMetrics(cfg)
	if err != nil {
		return nil, fmt.Errorf("error fetching kubelet metrics: %v", err)
	}
	kubernetesResources, err := fetchKubernetesResources(cfg)
	if err != nil {
		return nil, fmt.Errorf("error fetching kubernetes resources: %v", err)
	}
	nodeLabels, err := fetchNodeLabels(cfg)
	if err != nil {
		return nil, fmt.Errorf("error fetching node labels: %v", err)
	}

	metricsMap, err := t.convertToMetrics(kubeletMetrics, kubernetesResources)
	if err != nil {
		return nil, fmt.Errorf("error generating metrics: %v", err)
	}
	klog.Infof("metrics map: %v", metricsMap)

	err = t.addLabelsToMetrics(metricsMap, nodeLabels)
	if err != nil {
		return nil, fmt.Errorf("error adding labels to metrics: %v", err)
	}

	err = t.computeCPUUsage(metricsMap)
	if err != nil {
		return nil, fmt.Errorf("error computing CPU usage: %v", err)
	}
	//TODO convert to list and return.
	return nil, nil
}

func (t *Telemetry) convertToMetrics(kubeletMetrics map[string]*io_prometheus_client.MetricFamily, kubernetesResources map[string]int) (map[string]Metric, error) {
	metrics := make(map[string]Metric)

	timestamp := time.Now().UnixNano() / (int64(time.Millisecond) / int64(time.Nanosecond))

	translationKubeletMetrics := map[string]string{
		"machine_cpu_cores":             "cluster:capacity_cpu_cores:sum",
		"machine_memory_bytes":          "cluster:capacity_memory_bytes:sum",
		"node_cpu_usage_seconds_total":  "cluster:cpu_usage_cores:sum",
		"node_memory_working_set_bytes": "cluster:memory_usage_bytes:sum",
		"kubelet_running_containers":    "cluster:usage:containers:sum",
	}
	for kname, tname := range translationKubeletMetrics {
		metricFamily, ok := kubeletMetrics[kname]
		if !ok {
			return nil, fmt.Errorf("unable to find %v in kubelet metrics", kname)
		}
		labels := []MetricLabel{
			MetricLabel{
				Name:  "_id",
				Value: t.clusterId,
			},
		}
		switch tname {
		case "cluster:capacity_cpu_cores:sum", "cluster:capacity_memory_bytes:sum":
			labels = append(labels,
				MetricLabel{
					Name:  "label_kubernetes_io_arch",
					Value: "TODO",
				},
				MetricLabel{
					Name:  "label_node_openshift_io_os_id",
					Value: "TODO",
				},
				MetricLabel{
					Name:  "label_beta_kubernetes_io_instance_type",
					Value: "TODO",
				})
		}
		metrics[tname] = Metric{
			Name:      tname,
			Labels:    labels,
			Value:     aggregateMetricValues(metricFamily.Metric),
			Timestamp: timestamp,
		}
	}

	//TODO the resources for each metric have a problem. cant be a map because I have duplicates!
	// so now I need some kind of families, or directly go to do everything here instead of separating by functions.
	// maybe I could have some kind of label maps or something.
	return metrics, nil
}

func (t *Telemetry) addLabelsToMetrics(metrics map[string]Metric, nodeLabels map[string]string) error {
	return nil
}

func (t *Telemetry) computeCPUUsage(metrics map[string]Metric) error {
	return nil
}

func convertMetricsToWriteRequest(metrics []Metric) *prompb.WriteRequest {
	var timeSeriesList []prompb.TimeSeries
	var metricMetadataList []prompb.MetricMetadata
	for _, metric := range metrics {
		labels := []prompb.Label{
			{Name: "__name__", Value: metric.Name},
		}
		for _, label := range metric.Labels {
			labels = append(labels, prompb.Label{
				Name:  label.Name,
				Value: label.Value,
			})
		}
		samples := []prompb.Sample{
			prompb.Sample{
				Value:     metric.Value,
				Timestamp: metric.Timestamp,
			},
		}

		timeSeriesList = append(timeSeriesList, prompb.TimeSeries{
			Labels:  labels,
			Samples: samples,
		})

		metricMetadataList = append(metricMetadataList, prompb.MetricMetadata{
			MetricFamilyName: metric.Name,
			Type:             prompb.MetricMetadata_COUNTER,
		})
	}
	return &prompb.WriteRequest{
		Timeseries: timeSeriesList,
		Metadata:   metricMetadataList,
	}
}
