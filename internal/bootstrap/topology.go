package bootstrap

type TopologyTarget struct {
	Class            string
	TmuxServerDomain string
	PresentationName string
}

type TopologyInventory struct {
	External []TopologyTarget
	Managed  []TopologyTarget
}

const driverConsoleTopologyClass = "driver_console"

// ProjectTopology is presentation-only inventory. Stable Driver identities in
// Plan remain the sole lifecycle/routing authority.
func ProjectTopology(plan Plan) TopologyInventory {
	out := TopologyInventory{}
	for _, actor := range plan.Actors {
		if actor.Role == "interactor" {
			out.External = append(out.External, TopologyTarget{Class: "project_interactor",
				TmuxServerDomain: ExternalTmuxServerDomain, PresentationName: actor.PresentationName})
		}
		if actor.Role == "orchestrator" {
			out.Managed = append(out.Managed, TopologyTarget{Class: "project_orchestrator",
				TmuxServerDomain: ManagedTmuxServerDomain, PresentationName: actor.PresentationName})
		}
	}
	out.Managed = append(out.Managed,
		TopologyTarget{Class: "control_plane", TmuxServerDomain: ManagedTmuxServerDomain, PresentationName: "flowbee"},
		TopologyTarget{Class: "dynamic_worker", TmuxServerDomain: ManagedTmuxServerDomain,
			PresentationName: "flowbee-worker-{model}-" + plan.ProjectID + "-{canonical-epic-slug}"})
	// The Driver console is a read-only operator convenience, not part of the
	// lifecycle or attach readiness boundary. Only project it when the approved
	// plan elected to include the optional presentation class.
	for _, group := range plan.Groups {
		for _, class := range group.MemberClasses {
			if class == driverConsoleTopologyClass {
				out.Managed = append(out.Managed, TopologyTarget{Class: driverConsoleTopologyClass,
					TmuxServerDomain: ManagedTmuxServerDomain, PresentationName: "tmux-driver"})
				return out
			}
		}
	}
	return out
}
