package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
)

func (s *Scenario) ValueChecks(name string) error {
	// and now just the usual.
	if s.Name == "" {
		s.Name = name
	}
	for i := range s.Hosts {
		if s.Hosts[i].Name == "" {
			return fmt.Errorf("[scenario %s][host %d] host name is empty", s.Name, i)
		}
		if s.Hosts[i].Cpus == 0 {
			s.Hosts[i].Cpus = defaultCPUs
		}
		if s.Hosts[i].Memory == 0 {
			s.Hosts[i].Memory = defaultMemory
		}
		//TODO networks
		if s.Hosts[i].Kickstart.Template == "" {
			return fmt.Errorf("[scenario %s][host %v] kickstart template is empty", s.Name, s.Hosts[i].Name)
		}
		if s.Hosts[i].Kickstart.Commit == "" {
			return fmt.Errorf("[scenario %s][host %v] kickstart commit ref is empty", s.Name, s.Hosts[i].Name)
		}
		//TODo scenario settings.
	}
	return nil
}

func (s *Scenario) PrepareKickstart(scenarioInfoDir, kickstartTemplatesDir string) error {
	for _, host := range s.Hosts {
		fullVmName := fmt.Sprintf("%s-%s", s.Name, host.Name)
		outFile := fmt.Sprintf("%s/%s/vms/%s/kickstart.ks", scenarioInfoDir, s.Name, host.Name)
		VmHostname := strings.ReplaceAll(fullVmName, ".", "-")

		kickstartTemplate, err := loadKickstart(fmt.Sprintf("%s/%s", kickstartTemplatesDir, host.Kickstart.Template))
		if err != nil {
			return err
		}

		kickstartContents, err := kickstartReplace(
			kickstartTemplate,
			host.Kickstart.Commit,
			VmHostname,
			"AuthorizedKeys",   //TODO esto de donde sale? de las claves de ssh que uso por ahi. y de donde las paso? esto viene del scenario_settings.sh. que me lo pasen por otro lado.
			"PublicIP",         //TODO esto se lo tengo que dar. pero esta ip cual es? aqui no la se todavia. esto tengo que pensarlo.
			"fipsCommand",      //TODO donde? esto no se ni de donde viene ni siquiera si sirve de algo. si hay fips tengo que poner esto: fips-mode-setup --enable. de lo contrario nada.
			"EnableMirror",     //TODO esto me lo tienen que pasar. es exclusivo de boot. esto es un flag.
			"RegistryHostname", //TODO esto es el hostname donde este binario esta andando. esto lo puedo sacar solo
		)
		if err != nil {
			return err
		}

		err = saveKickstart(outFile, kickstartContents)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Scenario) Boot() {
	conn, err := libvirt.NewConnect("qemu:///system")
	if err != nil {
		panic(err)
	}
	//TODO need this before launching hosts.
	// 	<pool type='dir'>
	//   <name>test</name>
	//   <uuid>f7197d80-c7cb-4196-b315-20002807730a</uuid>
	//   <capacity unit='bytes'>8259158016</capacity>
	//   <allocation unit='bytes'>138829824</allocation>
	//   <available unit='bytes'>8120328192</available>
	//   <source>
	//   </source>
	//   <target>
	//     <path>/tmp/pool</path>
	//     <permissions>
	//       <mode>0711</mode>
	//       <owner>0</owner>
	//       <group>0</group>
	//       <label>system_u:object_r:virt_tmp_t:s0</label>
	//     </permissions>
	//   </target>
	// </pool>
	for _, host := range s.Hosts {
		//TODO name should be the full name.
		domain := libvirtxml.Domain{
			Name: host.Name,
			Type: "kvm",
			Memory: &libvirtxml.DomainMemory{
				Unit:  "MiB",
				Value: host.Memory,
			},
			VCPU: &libvirtxml.DomainVCPU{
				Placement: "static",
				Value:     host.Cpus,
			},
			Features: &libvirtxml.DomainFeatureList{
				ACPI: &libvirtxml.DomainFeature{},
				APIC: &libvirtxml.DomainFeatureAPIC{},
			},
			OnReboot: "restart",
			OS: &libvirtxml.DomainOS{
				Type: &libvirtxml.DomainOSType{
					Arch: "x86_64",
					Type: "hvm",
				},
				BootDevices: []libvirtxml.DomainBootDevice{
					{
						Dev: "cdrom",
					},
					// {
					// 	Dev: "hd",
					// },
				},
			},
			Devices: &libvirtxml.DomainDeviceList{
				Disks: []libvirtxml.DomainDisk{
					{
						Device: "cdrom",
						Driver: &libvirtxml.DomainDiskDriver{
							Name: "qemu",
							Type: "raw",
						},
						Source: &libvirtxml.DomainDiskSource{
							File: &libvirtxml.DomainDiskSourceFile{
								File: "/home/pacevedo/Downloads/rhel-9.2-x86_64-dvd.iso",
							},
						},
						ReadOnly: &libvirtxml.DomainDiskReadOnly{},
						Target: &libvirtxml.DomainDiskTarget{
							Dev: "sda",
							Bus: "sata",
						},
						Address: &libvirtxml.DomainAddress{
							Drive: &libvirtxml.DomainAddressDrive{
								Controller: &(&struct{ x uint }{0}).x,
								Bus:        &(&struct{ x uint }{0}).x,
								Target:     &(&struct{ x uint }{0}).x,
							},
						},
					},
					// {
					// 	Device: "disk",
					// 	Driver: &libvirtxml.DomainDiskDriver{
					// 		Name:    "qemu",
					// 		Type:    "qcow2",
					// 		Discard: "unmap",
					// 	},
					// 	Source: &libvirtxml.DomainDiskSource{
					// 		File: &libvirtxml.DomainDiskSourceFile{
					// 			File: "/tmp/test.qcow2",
					// 		},
					// 	},
					// 	Target: &libvirtxml.DomainDiskTarget{
					// 		Dev: "vda",
					// 		Bus: "virtio",
					// 	},
					// },
				},
				Interfaces: []libvirtxml.DomainInterface{
					{
						Source: &libvirtxml.DomainInterfaceSource{
							Network: &libvirtxml.DomainInterfaceSourceNetwork{
								Network: "default",
							},
						},
						Model: &libvirtxml.DomainInterfaceModel{
							Type: "virtio",
						},
					},
				},
				Consoles: []libvirtxml.DomainConsole{
					{
						Target: &libvirtxml.DomainConsoleTarget{
							Type: "serial",
							Port: &(&struct{ x uint }{0}).x,
						},
					},
				},
				//TODO this is optional.
				Graphics: []libvirtxml.DomainGraphic{
					{
						VNC: &libvirtxml.DomainGraphicVNC{
							Listen: "0.0.0.0",
						},
					},
				},
			},
		}
		xml, err := domain.Marshal()
		if err != nil {
			panic(err)
		}
		d, err := conn.DomainCreateXML(xml, 0)
		if err != nil {
			panic(err)
		}
		fmt.Println(d)
	}
}

func loadKickstart(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	return string(data), nil
}

func saveKickstart(path, contents string) error {
	dir := filepath.Dir(path)
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(contents)
	if err != nil {
		return err
	}
	return nil
}

func kickstartReplace(kickstartTemplate, commitRef, vmHostname, authorizedKeys, publicIP, fipsCommand, enableMirror, hostname string) (string, error) {
	regexes := []struct {
		Regex       *regexp.Regexp
		Replacement string
	}{
		{
			Regex:       regexp.MustCompile(`REPLACE_LVM_SYSROOT_SIZE`),
			Replacement: fmt.Sprintf("%v", "LvmRootSize"),
		},
		{
			Regex:       regexp.MustCompile(`REPLACE_OSTREE_SERVER_URL`),
			Replacement: "WebServerUrl",
		},
		{
			Regex:       regexp.MustCompile(`REPLACE_BOOT_COMMIT_REF`),
			Replacement: commitRef,
		},
		{
			Regex:       regexp.MustCompile(`REPLACE_PULL_SECRET`),
			Replacement: "PullSecret",
		},
		{
			Regex:       regexp.MustCompile(`REPLACE_HOST_NAME`),
			Replacement: hostname,
		},
		{
			Regex:       regexp.MustCompile(`REPLACE_REDHAT_AUTHORIZED_KEYS`),
			Replacement: authorizedKeys,
		},
		{
			Regex:       regexp.MustCompile(`REPLACE_PUBLIC_IP`),
			Replacement: publicIP,
		},
		{
			Regex:       regexp.MustCompile(`REPLACE_FIPS_COMMAND`),
			Replacement: fipsCommand,
		},
		{
			Regex:       regexp.MustCompile(`REPLACE_ENABLE_MIRROR`),
			Replacement: enableMirror,
		},
		{
			Regex:       regexp.MustCompile(`REPLACE_MIRROR_HOSTNAME`),
			Replacement: hostname,
		},
	}

	lines := kickstartTemplate
	for _, regex := range regexes {
		lines = regex.Regex.ReplaceAllString(lines, regex.Replacement)
	}

	return lines, nil
}

func GetScenarioByName(name string) (*Scenario, error) {
	if data, ok := testScenarios[name]; ok {
		return &data, nil
	}
	return nil, fmt.Errorf("scenario %s not found", name)
}
