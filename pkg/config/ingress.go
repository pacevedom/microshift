package config

type IngressStatusEnum string

const (
	StatusEnabled  = "Enabled"
	StatusDisabled = "Disabled"
)

type IngressConfig struct {
	Status             IngressStatusEnum        `json:"status"`
	Ports              IngressPolicyPortsConfig `json:"ports"`
	Expose             []string                 `json:"expose"`
	ServingCertificate []byte
	ServingKey         []byte
}

type IngressPolicyPortsConfig struct {
	Http  uint16 `json:"http"`
	Https uint16 `json:"https"`
}
