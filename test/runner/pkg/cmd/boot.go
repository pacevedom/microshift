package cmd

import (
	"github.com/openshift/microshift/test/runner/pkg/scenario"
	"github.com/spf13/cobra"
)

func NewBootCommand(scenarioInfoDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "boot",
		Short: "Boot scenarios",
	}

	var kickstartTemplatesDir string
	var scenarioName string

	flags := cmd.Flags()
	flags.StringVar(&kickstartTemplatesDir, "ks-templates-dir", "", "Kickstart templates directory")
	//TODO need periodics and presubmits too.
	flags.StringVar(&scenarioName, "scenario", "", "Scenario to run")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		//TODO check all flags have a value
		scn, err := scenario.GetScenarioByName(scenarioName)
		if err != nil {
			return err
		}
		//TODO scenario settings defaults
		err = scn.ValueChecks(scenarioName)
		if err != nil {
			return err
		}
		err = scn.PrepareKickstart(*scenarioInfoDir, kickstartTemplatesDir)
		if err != nil {
			return err
		}
		// and now that I have the kickstart, what? time to boot it. for this I am going to need the actual images.
		// I also need a webserver but that will come.
		scn.Boot()
		return nil
	}

	return cmd
}
