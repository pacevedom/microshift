package main

import (
	"os"

	cmds "github.com/openshift/microshift/test/runner/pkg/cmd"
	"github.com/spf13/cobra"
	"k8s.io/component-base/cli"
)

// const (
// 	LvmRootSize      = 10240
// 	WebServerUrl     = ""
// 	PullSecret       = ""
// 	AuthorizedKeys   = "" //TODO esto viene de leer ssh public key que me pasan por parametro.
// 	PublicIP         = "" //TODO esto que es?
// 	EnableMirror     = "" //TODO esto viene de fuera
// 	RegistryHostname = "" //tODO esto viene de fuera
// )

// func main() {
// 	conn, err := libvirt.NewConnect("qemu:///system")
// 	if err != nil {
// 		panic(err)
// 	}
// 	defer conn.Close()
// 	doms, err := conn.ListAllDomains(libvirt.CONNECT_LIST_DOMAINS_ACTIVE)
// 	if err != nil {
// 		panic(err)
// 	}

// 	fmt.Printf("%d running domains:\n", len(doms))
// 	for _, dom := range doms {
// 		name, err := dom.GetName()
// 		if err == nil {
// 			fmt.Printf("  %s\n", name)
// 		}
// 		dom.Free()
// 	}
// 	///////
// 	// tengo que resolver de donde saco todas las variables. tienen que ir como flags obviamente. como hago esto? cuales son esas variables uqe necesito?
// 	// SCENARIO_INFO_DIR la primera. esto es fijo durante toda la ejecucion.
// 	// KICKSTART_TEMPLATE_DIR. lo mismo. depende de la anterior, de hecho.
// 	// ENABLE_REGISTRY_MIRROR. viene de fuera.
// 	// en realidad necesitaria organizar esto un poco mejor.
// 	// que tendria en pkg? el scenario basico. en cmd el lanzador y poco mas? creoq eu si.

// 	fmt.Println(kickstartReplace("/home/pacevedo/go/src/github.com/pacevedom/microshift/test/kickstart-templates/kickstart.ks.template"))
// }

func main() {
	command := newCommand()
	code := cli.Run(command)
	os.Exit(code)
}

func newCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runner",
		Short: "Runner, a simple test runner for MicroShift",
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help() // err is always nil
			os.Exit(1)
		},
	}
	originalHelpFunc := cmd.HelpFunc()
	cmd.SetHelpFunc(func(command *cobra.Command, strings []string) {
		originalHelpFunc(command, strings)
	})

	scenarioInfoDir := cmd.PersistentFlags().String("scenario-info-dir", "", "Scenario info directory")

	//TODO need to pass flags around.
	cmd.AddCommand(cmds.NewBootCommand(scenarioInfoDir))
	return cmd
}
