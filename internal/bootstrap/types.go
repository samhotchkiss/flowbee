// Package bootstrap contains the deterministic, non-fork orchestration core for
// bringing a bare Flowbee installation to an attachable project state. It owns
// ordering and idempotency only. Every environment/product choice (cwd, how to
// start Driver, first Interactor command, and tmux grouping) remains behind an
// injected port; this package has no production DB, listener, tmux, or process
// implementation.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

type EndpointPurpose string

const (
	EndpointExternalInteractor EndpointPurpose = "external_interactor"
	EndpointManagedFleet       EndpointPurpose = "managed_fleet"
	ExternalTmuxServerDomain                   = "default"
	ManagedTmuxServerDomain                    = "flowbee"
)

type DriverEnsureMechanism string

const DriverEnsureManagedLaunchdTD DriverEnsureMechanism = "managed_launchd_td_ensure"

type EndpointRef struct {
	Purpose                 EndpointPurpose
	InstanceRef             string
	HostID                  string
	StoreID                 string
	TmuxServerDomainID      string
	EnsureMechanism         DriverEnsureMechanism
	ServiceManagerPath      string
	ServiceManagerSHA256    string
	ServiceUpdateAuthorized bool
	ReleaseID               string
	ExecutablePath          string
	ExecutableSHA256        string
	ConfigPath              string
	ConfigSHA256            string
	UDSPath                 string
	RequiredContracts       map[string]string
}

func (e EndpointRef) key() string {
	return string(e.Purpose) + ":" + e.HostID + ":" + e.StoreID + ":" + e.TmuxServerDomainID
}

type ControlPlaneSpec struct{ ID string }

type ActorOperation string

const (
	ActorAdopt  ActorOperation = "adopt"
	ActorEnsure ActorOperation = "ensure"
)

type ActorSpec struct {
	ID        string
	Role      string
	Operation ActorOperation
	Endpoint  EndpointRef
	// ExistingSessionID is required for Interactor adoption. The core refuses a
	// create operation until Driver's Ensure-v3 bootstrap/human-visible contract
	// exists; it never invents a session or first command.
	ExistingSessionID string
	PresentationName  string
}

type GroupSpec struct {
	ID                 string
	TmuxServerDomainID string
	ActorIDs           []string
	// MemberClasses is the closed managed-dedicated topology. It describes
	// presentation grouping only and is never routing authority.
	MemberClasses []string
}

type SeatSpec struct {
	ID       string
	Endpoint EndpointRef
}

type AttachIntentSpec struct {
	ID                 string
	InteractorActorID  string
	TmuxServerDomainID string
	PresentationName   string
}

// Plan is an operator-approved bootstrap contract. It deliberately contains no
// cwd algorithm, Driver launch command, agent command, or tmux grouping policy;
// those are represented only by opaque desired facts consumed by ports.
type Plan struct {
	ID           string
	ProjectID    string
	Endpoints    []EndpointRef
	ControlPlane ControlPlaneSpec
	Actors       []ActorSpec
	Groups       []GroupSpec
	LocalSeats   []SeatSpec
	AttachIntent AttachIntentSpec
}

type EffectRequest struct {
	ActionID  string
	ProjectID string
	CWD       string
}

type EffectReceipt struct {
	ID    string
	State string
}

type ProjectActivation struct {
	ProjectID        string
	Exists           bool
	BootstrapAllowed bool
	LiveReady        bool
	Holds            []string
}

type DriverPort interface {
	EndpointReady(context.Context, EndpointRef) (bool, error)
	// EnsureEndpoint is implemented by the approved managed launchd/td Ensure
	// boundary. The core never shells out or starts a daemon itself.
	EnsureEndpoint(context.Context, EndpointRef, EffectRequest) (EffectReceipt, error)
}

type ControlPlanePort interface {
	LiveReady(context.Context, ControlPlaneSpec) (bool, error)
	Ensure(context.Context, ControlPlaneSpec, EffectRequest) (EffectReceipt, error)
}

type ProjectActivationPort interface {
	ExactProjectActivation(context.Context, string) (ProjectActivation, error)
}

type ProjectInit struct {
	ProjectID        string
	RepositoryOrigin string
	CWD              string
}

type ProjectInitResolver interface {
	// ResolveProjectInit consumes the approved machine-local project marker. The
	// state machine injects it so the orchestration core never guesses from CWD.
	ResolveProjectInit(context.Context, string) (ProjectInit, error)
}

type ProjectProvisionPort interface {
	ProjectExists(context.Context, ProjectInit) (bool, error)
	AddProject(context.Context, ProjectInit, EffectRequest) (EffectReceipt, error)
	RepositoryAttached(context.Context, ProjectInit) (bool, error)
	AttachRepository(context.Context, ProjectInit, EffectRequest) (EffectReceipt, error)
}

type ActorPort interface {
	ActorReady(context.Context, ActorSpec) (bool, error)
	EnsureOrAdoptActor(context.Context, ActorSpec, EffectRequest) (EffectReceipt, error)
}

type SessionGroupPort interface {
	GroupReady(context.Context, GroupSpec) (bool, error)
	// EnsureGroup applies the approved dedicated `flowbee` tmux-server grouping
	// intent through an injected abstraction; no raw tmux path exists here.
	EnsureGroup(context.Context, GroupSpec, EffectRequest) (EffectReceipt, error)
}

type SeatPort interface {
	SeatBound(context.Context, SeatSpec) (bool, error)
	BindLocalSeat(context.Context, SeatSpec, EffectRequest) (EffectReceipt, error)
}

type InteractorPort interface {
	IntentAttached(context.Context, AttachIntentSpec) (bool, error)
	// AttachIntent may target only an existing adopted Interactor in the `default`
	// domain. Creating/spawning a first Interactor remains fail-closed.
	AttachIntent(context.Context, AttachIntentSpec, EffectRequest) (EffectReceipt, error)
}

type Ports struct {
	Driver     DriverPort
	Control    ControlPlanePort
	Activation ProjectActivationPort
	Init       ProjectInitResolver
	Projects   ProjectProvisionPort
	Actors     ActorPort
	Groups     SessionGroupPort
	Seats      SeatPort
	Interactor InteractorPort
}

type Checkpoint struct {
	BootstrapID      string
	PlanSHA256       string
	ProjectID        string
	Version          int64
	CWD              string
	RepositoryOrigin string
	Prepared         map[string]string
	Issued           map[string]string
	Completed        map[string]string
	LastHold         string
	Done             bool
}

type CheckpointStore interface {
	Load(context.Context, string) (Checkpoint, bool, error)
	Create(context.Context, Checkpoint) (Checkpoint, error)
	CompareAndSwap(context.Context, Checkpoint, int64) (Checkpoint, error)
}

var ErrCheckpointConflict = errors.New("bootstrap checkpoint conflict")

type Result struct {
	BootstrapID string
	Phase       string
	Complete    bool
	Hold        string
	Version     int64
}

func validatePlan(plan Plan, ports Ports) error {
	if strings.TrimSpace(plan.ID) == "" || strings.TrimSpace(plan.ProjectID) == "" ||
		strings.TrimSpace(plan.ControlPlane.ID) == "" || strings.TrimSpace(plan.AttachIntent.ID) == "" {
		return errors.New("bootstrap plan requires stable id, exact project, control plane, and attach intent")
	}
	if ports.Driver == nil || ports.Control == nil || ports.Activation == nil || ports.Init == nil ||
		ports.Projects == nil || ports.Actors == nil || ports.Groups == nil || ports.Seats == nil ||
		ports.Interactor == nil {
		return errors.New("bootstrap requires every injected port")
	}
	if len(plan.Endpoints) != 2 {
		return errors.New("bootstrap requires exactly external and managed Driver endpoints")
	}
	purposes := map[EndpointPurpose]bool{}
	keys := map[string]bool{}
	for _, endpoint := range plan.Endpoints {
		if endpoint.InstanceRef == "" || endpoint.HostID == "" || endpoint.StoreID == "" || endpoint.TmuxServerDomainID == "" {
			return errors.New("Driver endpoint identity must be exact host/store/domain")
		}
		if endpoint.Purpose != EndpointExternalInteractor && endpoint.Purpose != EndpointManagedFleet {
			return fmt.Errorf("unknown Driver endpoint purpose %q", endpoint.Purpose)
		}
		if endpoint.EnsureMechanism != DriverEnsureManagedLaunchdTD {
			return errors.New("Driver endpoint must use the approved managed launchd/td Ensure boundary")
		}
		if !filepath.IsAbs(endpoint.ServiceManagerPath) || !validSHA256(endpoint.ServiceManagerSHA256) ||
			endpoint.ReleaseID == "" || !filepath.IsAbs(endpoint.ExecutablePath) ||
			!validSHA256(endpoint.ExecutableSHA256) || !filepath.IsAbs(endpoint.ConfigPath) ||
			!validSHA256(endpoint.ConfigSHA256) || !filepath.IsAbs(endpoint.UDSPath) ||
			len(endpoint.RequiredContracts) == 0 {
			return errors.New("Driver service Ensure requires exact release, executable/config paths+hashes, UDS, and contracts")
		}
		if endpoint.Purpose == EndpointExternalInteractor && endpoint.TmuxServerDomainID != ExternalTmuxServerDomain {
			return errors.New("external Interactor endpoint must use the default tmux server domain")
		}
		if endpoint.Purpose == EndpointManagedFleet && endpoint.TmuxServerDomainID != ManagedTmuxServerDomain {
			return errors.New("managed actors and seats must use the dedicated flowbee tmux server domain")
		}
		if purposes[endpoint.Purpose] || keys[endpoint.key()] {
			return errors.New("Driver endpoint purposes and identities must be distinct")
		}
		purposes[endpoint.Purpose], keys[endpoint.key()] = true, true
	}
	if !purposes[EndpointExternalInteractor] || !purposes[EndpointManagedFleet] {
		return errors.New("both external and managed Driver endpoint domains are required")
	}
	actorIDs := map[string]bool{}
	roles := map[string]bool{}
	actorByID := map[string]ActorSpec{}
	interactorID := ""
	for _, actor := range plan.Actors {
		if actor.ID == "" || (actor.Role != "interactor" && actor.Role != "orchestrator") ||
			(actor.Operation != ActorAdopt && actor.Operation != ActorEnsure) || !keys[actor.Endpoint.key()] {
			return errors.New("actor requires stable id, role, operation, and exact planned endpoint")
		}
		if actorIDs[actor.ID] || roles[actor.Role] {
			return errors.New("bootstrap requires one distinct Interactor and Orchestrator")
		}
		actorIDs[actor.ID], roles[actor.Role] = true, true
		actorByID[actor.ID] = actor
		switch actor.Role {
		case "interactor":
			if actor.Operation != ActorAdopt || actor.Endpoint.Purpose != EndpointExternalInteractor ||
				actor.ExistingSessionID == "" || actor.PresentationName != plan.ProjectID+"-interactor" {
				return errors.New("Interactor create is held; bootstrap requires exact existing-session adoption")
			}
			interactorID = actor.ID
		case "orchestrator":
			if actor.Operation != ActorEnsure || actor.Endpoint.Purpose != EndpointManagedFleet ||
				actor.ExistingSessionID != "" || actor.PresentationName != plan.ProjectID+"-orchestrator" {
				return errors.New("Orchestrator must be ensured in the dedicated managed domain")
			}
		}
	}
	if len(plan.Actors) != 2 || !roles["interactor"] || !roles["orchestrator"] ||
		plan.AttachIntent.InteractorActorID != interactorID ||
		plan.AttachIntent.TmuxServerDomainID != ExternalTmuxServerDomain {
		return errors.New("attach intent must target the planned Interactor")
	}
	seen := map[string]bool{}
	for _, seat := range plan.LocalSeats {
		if seat.ID == "" || seen[seat.ID] || !keys[seat.Endpoint.key()] ||
			seat.Endpoint.Purpose != EndpointManagedFleet {
			return errors.New("local seat requires unique id and exact planned endpoint")
		}
		seen[seat.ID] = true
	}
	for _, group := range plan.Groups {
		if group.ID == "" || seen["group:"+group.ID] || len(group.ActorIDs) == 0 ||
			group.TmuxServerDomainID != ManagedTmuxServerDomain {
			return errors.New("session group requires stable id and actor members")
		}
		seen["group:"+group.ID] = true
		for _, actorID := range group.ActorIDs {
			if !actorIDs[actorID] || actorByID[actorID].Endpoint.Purpose != EndpointManagedFleet {
				return errors.New("session group names an actor outside the plan")
			}
		}
		classes := map[string]bool{}
		for _, class := range group.MemberClasses {
			classes[class] = true
		}
		for _, required := range []string{"control_plane", "project_orchestrator", "dynamic_worker"} {
			if !classes[required] {
				return errors.New("managed flowbee topology is missing a required presentation class")
			}
		}
		for class := range classes {
			if class != "control_plane" && class != "project_orchestrator" &&
				class != "dynamic_worker" && class != driverConsoleTopologyClass {
				return errors.New("only approved managed presentation classes may enter the flowbee server domain")
			}
		}
		if len(classes) < 3 || len(classes) > 4 {
			return errors.New("managed flowbee topology has invalid presentation classes")
		}
	}
	if plan.AttachIntent.PresentationName != plan.ProjectID+"-interactor" {
		return errors.New("human attach must use the reserved project Interactor presentation name")
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, r := range value[len("sha256:"):] {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func sortedPlan(plan Plan) Plan {
	out := plan
	out.Endpoints = append([]EndpointRef(nil), plan.Endpoints...)
	out.Actors = append([]ActorSpec(nil), plan.Actors...)
	out.Groups = append([]GroupSpec(nil), plan.Groups...)
	out.LocalSeats = append([]SeatSpec(nil), plan.LocalSeats...)
	for i := range out.Groups {
		out.Groups[i].ActorIDs = append([]string(nil), out.Groups[i].ActorIDs...)
		out.Groups[i].MemberClasses = append([]string(nil), out.Groups[i].MemberClasses...)
		sort.Strings(out.Groups[i].ActorIDs)
		sort.Strings(out.Groups[i].MemberClasses)
	}
	sort.Slice(out.Endpoints, func(i, j int) bool { return out.Endpoints[i].key() < out.Endpoints[j].key() })
	sort.Slice(out.Actors, func(i, j int) bool {
		if out.Actors[i].Role != out.Actors[j].Role {
			return out.Actors[i].Role == "orchestrator"
		}
		return out.Actors[i].ID < out.Actors[j].ID
	})
	sort.Slice(out.Groups, func(i, j int) bool { return out.Groups[i].ID < out.Groups[j].ID })
	sort.Slice(out.LocalSeats, func(i, j int) bool { return out.LocalSeats[i].ID < out.LocalSeats[j].ID })
	return out
}
