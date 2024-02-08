package config

type IngressStatusEnum string
type NamespaceOwnershipEnum string

const (
	StatusEnabled             = "Enabled"
	StatusDisabled            = "Disabled"
	NamespaceOwnershipStrict  = "Strict"
	NamespaceOwnershipAllowed = "InterNamespaceAllowed"
)

type IngressConfig struct {
	Status             IngressStatusEnum        `json:"status"`
	Ports              IngressPolicyPortsConfig `json:"ports"`
	Expose             ExposeConfig             `json:"expose"`
	AdmissionPolicy    RouteAdmissionPolicy     `json:"routeAdmissionPolicy"`
	ServingCertificate []byte
	ServingKey         []byte
}

type IngressPolicyPortsConfig struct {
	Http  int `json:"http"`
	Https int `json:"https"`
}

type RouteAdmissionPolicy struct {
	NamespaceOwnership NamespaceOwnershipEnum `json:"namespaceOwnership"`
}

type ExposeConfig struct {
	Hostnames   []string `json:"hostnames"`
	Interfaces  []string `json:"interfaces"`
	IPAddresses []string `json:"ipAddresses"`
}
