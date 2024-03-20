package scenario

//TODO periodics and presubmits somewhere? That might be a good thing to have.
// how to organize it?

var testScenarios = map[string]Scenario{
	"el92-base@upgrade-ok": Scenario{
		Hosts: []ScenarioHost{
			{
				Name: "host1",
				Kickstart: ScenarioHostKickstart{
					Commit:   "rhel-9.2-microshift-source-base",
					Template: "kickstart.ks.template",
				},
			},
		},
	},
}
