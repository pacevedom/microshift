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

	"github.com/golang/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
	"k8s.io/klog/v2"
)

const (
	authString = `{"authorization_token": "%s", "cluster_id": "%s"}`
)

type Metric struct {
	Name    string         `json:"name"`
	Labels  []MetricLabel  `json:"labels"`
	Samples []MetricSample `json:"samples"`
}

type MetricLabel struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type MetricSample struct {
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"`
}

type Telemetry struct {
	encodedAuth string
	endpoint    string
}

func NewTelemetry(baseURL, clusterId, pullSecret string) *Telemetry {
	authString := fmt.Sprintf(authString, pullSecret, clusterId)
	encodedAuth := base64.StdEncoding.EncodeToString([]byte(authString))
	return &Telemetry{
		encodedAuth: encodedAuth,
		endpoint:    fmt.Sprintf("%s/metrics/v1/receive", baseURL),
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
		samples := []prompb.Sample{}
		for _, sample := range metric.Samples {
			samples = append(samples, prompb.Sample{
				Value:     sample.Value,
				Timestamp: sample.Timestamp,
			})
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
