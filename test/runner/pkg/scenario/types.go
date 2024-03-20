package scenario

const (
	// Default number of CPUs for a VM.
	defaultCPUs = 2
	// Default memory allocation for a VM. MB.
	defaultMemory = 4096
	// Default disk size for a VM. GB.
	defaultDiskSize = 20
)

type Scenario struct {
	Name  string
	Hosts []ScenarioHost
}

type ScenarioSettings struct {
	LvmRootSize          int
	OsTreeServerURL      string
	PullSecrets          string
	RedHatAuthorizedKeys string
	MirrorHostname       string
}

type ScenarioHost struct {
	Name      string
	Cpus      uint
	Memory    uint
	Networks  []ScenarioHostNetwork
	DiskSize  int
	Fips      bool
	Kickstart ScenarioHostKickstart
	//TODO should I have more parameters here? like hostnames or whatever? yeah I should have some of those.
}

type ScenarioHostNetwork struct {
	// name and number of nics. this neednt be an array outside.
}

type ScenarioHostKickstart struct {
	Template string
	Commit   string
}
