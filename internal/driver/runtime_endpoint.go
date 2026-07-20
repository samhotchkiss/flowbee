package driver

import "errors"

// resolveRuntimePort selects the one endpoint authorized by an action's
// immutable host/store/tmux-server-domain tuple. When an inventory is present,
// a missing tuple is terminal for this attempt: the legacy single Port is never
// consulted as a fallback.
//
// Port remains only as a compatibility seam while serve/tests are migrated to
// construct an EndpointResolver. Production multi-endpoint activation requires
// Resolver; callers must not populate both fields expecting failover semantics.
func resolveRuntimePort(resolver *EndpointResolver, legacy DriverPort, action Action) (DriverPort, error) {
	if resolver != nil {
		endpoint, err := resolver.ResolveAction(action)
		if err != nil {
			return nil, err
		}
		if nilDriverPort(endpoint.Port) {
			return nil, ErrEndpointNotFound
		}
		return endpoint.Port, nil
	}
	if nilDriverPort(legacy) {
		return nil, errors.New("driver runtime requires endpoint resolver")
	}
	return legacy, nil
}

// resolveRuntimeSendPort applies the additional live control-principal proof to
// new Flowbee-authored sends. Read-only receipt verification deliberately uses
// resolveRuntimePort so a revoked send scope cannot strand uncertain recovery.
func resolveRuntimeSendPort(resolver *EndpointResolver, legacy DriverPort, action Action) (DriverPort, error) {
	if resolver != nil && action.SenderPrincipalID != "" {
		endpoint, err := resolver.ResolveControlAction(action)
		if err != nil {
			return nil, err
		}
		if nilDriverPort(endpoint.Port) {
			return nil, ErrEndpointNotFound
		}
		return endpoint.Port, nil
	}
	return resolveRuntimePort(resolver, legacy, action)
}

// validateSessionOriginEndpoint prevents an A->B session route from crossing a
// Driver store or tmux-server isolation domain. Control-origin actions have no
// session sender and are authorized by the resolved endpoint's principal
// capability instead.
func validateSessionOriginEndpoint(action Action) error {
	if action.SenderPrincipalID != "" {
		return nil
	}
	if action.SenderHostID == "" || action.SenderStoreID == "" ||
		action.SenderServerDomainID == "" || action.SenderServerID == "" {
		return errors.New("driver session-origin action missing sender endpoint identity")
	}
	if action.SenderHostID != action.TargetHostID ||
		action.SenderStoreID != action.TargetStoreID ||
		action.SenderServerDomainID != action.TargetServerDomainID ||
		action.SenderServerID != action.TargetServerID {
		return ErrIdentityMismatch
	}
	return nil
}

func controlRecipientRunFence(action Action) string {
	if action.SenderPrincipalID != "" {
		return action.RecipientAgentRunID
	}
	return ""
}
