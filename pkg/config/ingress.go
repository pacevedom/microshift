package config

type IngressStatusEnum string

const (
	StatusEnabled  = "Enabled"
	StatusDisabled = "Disabled"
)

type IngressConfig struct {
	Status             IngressStatusEnum   `json:"status"`
	Policy             IngressPolicyConfig `json:"policy"`
	ServingCertificate []byte
	ServingKey         []byte
}

type IngressPolicyConfig struct {
	Ports  IngressPolicyPortsConfig `json:"ports"`
	Expose []string                 `json:"expose"`
	Allow  []string                 `json:"allow,omitempty"`
}

type IngressPolicyPortsConfig struct {
	Http  IngressPolicyPort `json:"http"`
	Https IngressPolicyPort `json:"https"`
}

type IngressPolicyPort struct {
	Status IngressStatusEnum `json:"status"`
	Port   uint16            `json:"port"`
}
