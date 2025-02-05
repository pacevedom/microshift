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
	"encoding/base64"
	"fmt"
)

const (
	authString = `{"authorization_token": "%s", "cluster_id": "%s"}`
)

type Telemetry struct {
	encodedAuth string
}

func NewTelemetry(clusterId, pullSecret string) *Telemetry {
	authString := fmt.Sprintf(authString, pullSecret, clusterId)
	encodedAuth := base64.StdEncoding.EncodeToString([]byte(authString))
	return &Telemetry{
		encodedAuth: encodedAuth,
	}
}
