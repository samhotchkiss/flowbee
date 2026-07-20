package driver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
)

var (
	// ErrEndpointNotFound is returned instead of falling back to another Driver
	// endpoint. A missing endpoint is a durable delivery hold, never permission
	// to cross a tmux-server isolation boundary.
	ErrEndpointNotFound = errors.New("driver endpoint not found")
	// ErrEndpointAmbiguous identifies inventory that would allow one exact
	// routing tuple to select more than one Driver endpoint.
	ErrEndpointAmbiguous = errors.New("driver endpoint is ambiguous")
	// ErrEndpointUnverified means exact inventory exists, but this process has
	// not completed a stable authenticated capability proof for its current tmux
	// server incarnation. Construction metadata alone is never send authority.
	ErrEndpointUnverified = errors.New("driver endpoint capability is not verified")
)

// EndpointKey is the complete Driver routing domain. All three fields are
// required; there is deliberately no process-wide or per-host default.
type EndpointKey struct {
	HostID             string
	StoreID            string
	TmuxServerDomainID string
}

func (k EndpointKey) validate() error {
	if k.HostID == "" || k.StoreID == "" || k.TmuxServerDomainID == "" {
		return errors.New("driver endpoint: incomplete exact key")
	}
	return nil
}

// EndpointEntry is explicit operator inventory for one authenticated Driver
// endpoint. ExpectedServerOwnership is the Driver server-domain ownership
// (external or managed_dedicated), not lifecycle ownership of an individual
// session.
type EndpointEntry struct {
	InstanceRef             string
	Port                    DriverPort
	Expected                EndpointKey
	ExpectedServerOwnership string
}

// ResolvedEndpoint is a metadata-verified endpoint snapshot. The exact routing
// key and server ownership are immutable inventory, while Metadata's tmux
// server instance may advance after a Driver-managed server replacement. Port
// is exposed so an executor can perform an operation only after resolving its
// action's exact EndpointKey and current server incarnation.
type ResolvedEndpoint struct {
	InstanceRef string
	Key         EndpointKey
	Metadata    DriverMetadata
	Port        DriverPort
}

// EndpointReadiness is scoped to one exact endpoint key. It must not be folded
// into a single global capability bit because external and managed-dedicated
// server domains are independent authorization boundaries.
type EndpointReadiness struct {
	Endpoint   ResolvedEndpoint
	Capability ControlOriginCapability
}

// EndpointResolver is safe for concurrent use. Its exact host/store/domain and
// ownership inventory is immutable after construction. Authenticated readiness
// probes may atomically advance only the cached tmux-server incarnation; this
// lets new actions recover after a managed server replacement while old actions
// remain fenced by their immutable TargetServerID.
type EndpointResolver struct {
	mu                sync.RWMutex
	byKey             map[EndpointKey]ResolvedEndpoint
	current           map[EndpointKey]bool
	controlAuthorized map[EndpointKey]bool
	probeMu           map[EndpointKey]*sync.Mutex
}

// NewEndpointResolver builds exact Driver inventory. Static ambiguity is
// rejected before any endpoint is probed, then live metadata must match the
// configured host, store, tmux-server domain, and server ownership exactly.
func NewEndpointResolver(ctx context.Context, entries []EndpointEntry) (*EndpointResolver, error) {
	if len(entries) == 0 {
		return nil, errors.New("driver endpoint inventory: empty")
	}

	instanceRefs := make(map[string]struct{}, len(entries))
	keys := make(map[EndpointKey]struct{}, len(entries))
	for _, entry := range entries {
		if entry.InstanceRef == "" || nilDriverPort(entry.Port) {
			return nil, errors.New("driver endpoint inventory: incomplete entry")
		}
		if err := entry.Expected.validate(); err != nil {
			return nil, err
		}
		switch entry.ExpectedServerOwnership {
		case "external":
			if entry.Expected.TmuxServerDomainID != "default" {
				return nil, errors.New("driver endpoint inventory: external server must use default domain")
			}
		case "managed_dedicated":
			if entry.Expected.TmuxServerDomainID == "default" {
				return nil, errors.New("driver endpoint inventory: managed server cannot use default domain")
			}
		default:
			return nil, errors.New("driver endpoint inventory: invalid expected server ownership")
		}
		if _, duplicate := instanceRefs[entry.InstanceRef]; duplicate {
			return nil, fmt.Errorf("%w: duplicate instance_ref %q", ErrEndpointAmbiguous, entry.InstanceRef)
		}
		instanceRefs[entry.InstanceRef] = struct{}{}
		if _, duplicate := keys[entry.Expected]; duplicate {
			return nil, fmt.Errorf("%w: duplicate exact key", ErrEndpointAmbiguous)
		}
		keys[entry.Expected] = struct{}{}
	}

	resolved := make(map[EndpointKey]ResolvedEndpoint, len(entries))
	for _, entry := range entries {
		metadata, err := entry.Port.Metadata(ctx)
		if err != nil {
			return nil, fmt.Errorf("driver endpoint %q metadata: %w", entry.InstanceRef, err)
		}
		if err := validateDriverMetadata(metadata); err != nil {
			return nil, fmt.Errorf("driver endpoint %q metadata: %w", entry.InstanceRef, err)
		}
		actual := EndpointKey{
			HostID:             metadata.HostID,
			StoreID:            metadata.StoreID,
			TmuxServerDomainID: metadata.TmuxServer.DomainID,
		}
		if err := actual.validate(); err != nil {
			return nil, fmt.Errorf("driver endpoint %q metadata: %w", entry.InstanceRef, err)
		}
		if actual != entry.Expected || metadata.TmuxServer.Ownership != entry.ExpectedServerOwnership {
			return nil, fmt.Errorf("driver endpoint %q metadata: %w", entry.InstanceRef, ErrIdentityMismatch)
		}
		resolved[actual] = ResolvedEndpoint{
			InstanceRef: entry.InstanceRef,
			Key:         actual,
			Metadata:    metadata,
			Port:        entry.Port,
		}
	}
	probeMu := make(map[EndpointKey]*sync.Mutex, len(resolved))
	for key := range resolved {
		probeMu[key] = &sync.Mutex{}
	}
	current := make(map[EndpointKey]bool, len(resolved))
	for key := range resolved {
		current[key] = true
	}
	return &EndpointResolver{byKey: resolved, current: current,
		controlAuthorized: make(map[EndpointKey]bool, len(resolved)), probeMu: probeMu}, nil
}

// Resolve selects only an exact complete key. It performs no network calls and
// has no default endpoint, host-only fallback, or store-only fallback.
func (r *EndpointResolver) Resolve(key EndpointKey) (ResolvedEndpoint, error) {
	if r == nil || r.byKey == nil {
		return ResolvedEndpoint{}, ErrEndpointNotFound
	}
	if err := key.validate(); err != nil {
		return ResolvedEndpoint{}, fmt.Errorf("%w: %v", ErrEndpointNotFound, err)
	}
	r.mu.RLock()
	endpoint, ok := r.byKey[key]
	r.mu.RUnlock()
	if !ok {
		return ResolvedEndpoint{}, ErrEndpointNotFound
	}
	return endpoint, nil
}

// ResolveAction applies the same exact boundary to an immutable Flowbee action.
func (r *EndpointResolver) ResolveAction(action Action) (ResolvedEndpoint, error) {
	key := EndpointKey{
		HostID:             action.TargetHostID,
		StoreID:            action.TargetStoreID,
		TmuxServerDomainID: action.TargetServerDomainID,
	}
	if r == nil || key.validate() != nil {
		return ResolvedEndpoint{}, ErrEndpointNotFound
	}
	r.mu.RLock()
	endpoint, ok := r.byKey[key]
	current := r.current[key]
	r.mu.RUnlock()
	if !ok {
		return ResolvedEndpoint{}, ErrEndpointNotFound
	}
	if action.TargetServerID == "" || action.TargetServerID != endpoint.Metadata.TmuxServer.InstanceID {
		return ResolvedEndpoint{}, ErrIdentityMismatch
	}
	if action.InstanceRef != "" && action.InstanceRef != endpoint.InstanceRef {
		return ResolvedEndpoint{}, ErrIdentityMismatch
	}
	if !current {
		return ResolvedEndpoint{}, ErrEndpointUnverified
	}
	return endpoint, nil
}

// ResolveControlAction adds the authenticated control-principal capability
// fence required for a new Flowbee-authored message send. Lifecycle operations
// and read-only receipt recovery deliberately use ResolveAction: they belong to
// separate Driver permission domains and must remain available when
// messages:send is revoked.
func (r *EndpointResolver) ResolveControlAction(action Action) (ResolvedEndpoint, error) {
	key := EndpointKey{HostID: action.TargetHostID, StoreID: action.TargetStoreID,
		TmuxServerDomainID: action.TargetServerDomainID}
	if r == nil || key.validate() != nil {
		return ResolvedEndpoint{}, ErrEndpointNotFound
	}
	r.mu.RLock()
	endpoint, ok := r.byKey[key]
	current, authorized := r.current[key], r.controlAuthorized[key]
	r.mu.RUnlock()
	if !ok {
		return ResolvedEndpoint{}, ErrEndpointNotFound
	}
	if action.TargetServerID == "" || action.TargetServerID != endpoint.Metadata.TmuxServer.InstanceID ||
		(action.InstanceRef != "" && action.InstanceRef != endpoint.InstanceRef) {
		return ResolvedEndpoint{}, ErrIdentityMismatch
	}
	if !current || !authorized {
		return ResolvedEndpoint{}, ErrEndpointUnverified
	}
	return endpoint, nil
}

// ControlReadiness probes authenticated control-origin capability only on the
// endpoint selected by the exact key. A missing key produces no Driver call.
func (r *EndpointResolver) ControlReadiness(ctx context.Context, key EndpointKey) (EndpointReadiness, error) {
	if r == nil {
		return EndpointReadiness{}, ErrEndpointNotFound
	}
	r.mu.RLock()
	endpointProbeMu := r.probeMu[key]
	r.mu.RUnlock()
	if endpointProbeMu == nil {
		return EndpointReadiness{}, ErrEndpointNotFound
	}
	endpointProbeMu.Lock()
	defer endpointProbeMu.Unlock()

	endpoint, err := r.Resolve(key)
	if err != nil {
		return EndpointReadiness{}, err
	}
	// A readiness probe is itself an authorization transition. Withdraw the
	// prior proof before any network call so every failure mode closes execution.
	r.invalidateEndpoint(key)
	metadata, err := endpoint.Port.Metadata(ctx)
	if err != nil {
		return EndpointReadiness{}, err
	}
	if err := validateDriverMetadata(metadata); err != nil {
		return EndpointReadiness{}, err
	}
	actual := EndpointKey{HostID: metadata.HostID, StoreID: metadata.StoreID, TmuxServerDomainID: metadata.TmuxServer.DomainID}
	if actual != endpoint.Key || metadata.TmuxServer.Ownership != endpoint.Metadata.TmuxServer.Ownership {
		return EndpointReadiness{}, ErrIdentityMismatch
	}
	// Every probe first withdraws send authority and publishes the latest exact
	// incarnation. A replacement therefore fences old durable actions even when
	// capability verification is temporarily unavailable.
	r.publishEndpointMetadata(key, metadata, false)
	if !metadata.ControlPrincipalOrigin {
		return EndpointReadiness{}, errors.New("driver endpoint: control principal origin is not advertised")
	}
	capability, err := endpoint.Port.ControlOriginCapability(ctx)
	if err != nil {
		return EndpointReadiness{}, err
	}
	if err := validateControlOriginCapability(capability); err != nil {
		return EndpointReadiness{}, err
	}
	// Bind capability to a stable incarnation. The HTTP capability call performs
	// its own metadata request, so explicitly reading metadata again prevents a
	// server replacement during the proof from authorizing the wrong instance.
	after, err := endpoint.Port.Metadata(ctx)
	if err != nil {
		return EndpointReadiness{}, err
	}
	if err := validateDriverMetadata(after); err != nil {
		return EndpointReadiness{}, err
	}
	afterKey := EndpointKey{HostID: after.HostID, StoreID: after.StoreID, TmuxServerDomainID: after.TmuxServer.DomainID}
	if afterKey != endpoint.Key || after.TmuxServer.Ownership != endpoint.Metadata.TmuxServer.Ownership {
		return EndpointReadiness{}, ErrIdentityMismatch
	}
	r.publishEndpointMetadata(key, after, false)
	if after.TmuxServer.InstanceID != metadata.TmuxServer.InstanceID {
		return EndpointReadiness{}, ErrIdentityMismatch
	}
	endpoint.Metadata = after
	r.publishEndpointMetadata(key, after, true)
	return EndpointReadiness{Endpoint: endpoint, Capability: capability}, nil
}

func (r *EndpointResolver) publishEndpointMetadata(key EndpointKey, metadata DriverMetadata, authorized bool) {
	r.mu.Lock()
	endpoint := r.byKey[key]
	endpoint.Metadata = metadata
	r.byKey[key] = endpoint
	r.current[key] = true
	r.controlAuthorized[key] = authorized
	r.mu.Unlock()
}

func (r *EndpointResolver) invalidateEndpoint(key EndpointKey) {
	r.mu.Lock()
	r.current[key] = false
	r.controlAuthorized[key] = false
	r.mu.Unlock()
}

func nilDriverPort(port DriverPort) bool {
	if port == nil {
		return true
	}
	value := reflect.ValueOf(port)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
