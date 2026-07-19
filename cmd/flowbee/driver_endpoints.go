package main

import (
	"context"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/driver"
)

const driverEndpointCallTimeout = 20 * time.Second

// serveDriverEndpoint is the non-secret runtime projection of one configured
// Driver isolation domain. The bearer remains encapsulated by its HTTP port.
type serveDriverEndpoint struct {
	InstanceRef string
	Key         driver.EndpointKey
	Port        driver.DriverPort
}

// loadServeDriverEndpoints constructs the exact endpoint resolver used by every
// v2 runtime. It never reads the legacy single-socket environment and never
// synthesizes a default endpoint. NewEndpointResolver authenticates live
// metadata and pins host/store/domain/ownership before this inventory is
// published to any executor.
func loadServeDriverEndpoints(ctx context.Context, inventory config.DriverEndpointInventory) ([]serveDriverEndpoint, *driver.EndpointResolver, error) {
	if err := inventory.Validate(); err != nil {
		return nil, nil, err
	}
	endpoints := make([]serveDriverEndpoint, 0, len(inventory.Endpoints))
	entries := make([]driver.EndpointEntry, 0, len(inventory.Endpoints))
	for _, configured := range inventory.Endpoints {
		token, err := readOwnerOnlySecret(configured.TokenFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read Driver endpoint %q token: %w", configured.InstanceRef, err)
		}
		port := driver.NewUDSPort(configured.UDSPath, token)
		key := driver.EndpointKey{
			HostID:             configured.ExpectedHostID,
			StoreID:            configured.ExpectedStoreID,
			TmuxServerDomainID: configured.ExpectedTmuxServerDomainID,
		}
		endpoints = append(endpoints, serveDriverEndpoint{InstanceRef: configured.InstanceRef, Key: key, Port: port})
		entries = append(entries, driver.EndpointEntry{
			InstanceRef:             configured.InstanceRef,
			Port:                    port,
			Expected:                key,
			ExpectedServerOwnership: configured.ExpectedTmuxServerOwnership,
		})
	}
	probeCtx, cancel := context.WithTimeout(ctx, driverEndpointCallTimeout)
	defer cancel()
	resolver, err := driver.NewEndpointResolver(probeCtx, entries)
	if err != nil {
		return nil, nil, fmt.Errorf("initialize exact Driver endpoint inventory: %w", err)
	}
	return endpoints, resolver, nil
}

func driverEndpointReconcilerName(kind, instanceRef string) string {
	return kind + "." + instanceRef
}
