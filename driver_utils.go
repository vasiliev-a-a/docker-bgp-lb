package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

var (
	ErrSubnetAlreadyAdvertised = errors.New("subnet is already advertised")
)

func loadState() (*bgpLB, error) {
	data, err := os.ReadFile(stateFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		// The state file does not exist yet - first run. Not an error.
		return &bgpLB{Networks: make(map[string]*bgpNetwork)}, nil
	}

	var b bgpLB
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}

	// Small guard if persisted state is corrupted, contains {"Networks": null}, or the field is absent.
	if b.Networks == nil {
		b.Networks = make(map[string]*bgpNetwork)
	}

	// Only `Networks` is restored; `ctx` and `scope` are runtime concerns
	// set by [initLBServer] (they are unexported and never persisted).
	return &b, nil
}

// lockState locks the persistable state of the plugin (the networks and their endpoints).
// It returns a function to unlock it, which must be called after the caller is done with the state.
// Primary use case is to ensure that the state is not modified while it is being saved to disk.
// Must never be called while holding a network mutex ([sync.Mutex] is not reentrant).
func (lb *bgpLB) lockState() func() {
	lb.networksMu.Lock()
	for i := range lb.Networks {
		lb.Networks[i].Lock()
	}

	return func() {
		for i := range lb.Networks {
			lb.Networks[i].Unlock()
		}
		lb.networksMu.Unlock()
	}
}

func (lb *bgpLB) saveState() error {
	unlock := lb.lockState()
	defer unlock()

	data, err := json.Marshal(lb)
	if err != nil {
		return err
	}
	return os.WriteFile(stateFile, data, 0644)
}

func (lb *bgpLB) isEndpointManaged(networkID, endpointID string) bool {
	lb.networksMu.Lock()

	bgpNetwork, netOk := lb.Networks[networkID]
	if !netOk {
		lb.networksMu.Unlock()
		return false
	}
	bgpNetwork.Lock()
	defer bgpNetwork.Unlock()
	lb.networksMu.Unlock()

	_, epOk := bgpNetwork.endpoints[endpointID]

	return epOk
}

func isNetworkAdvertised(netID string) bool {
	lbServer.advertisedNetworksMu.Lock()
	defer lbServer.advertisedNetworksMu.Unlock()

	_, ok := lbServer.advertisedNetworks[netID]

	return ok
}

func addAdvertisedSubnet(ctx context.Context, netID, subnet string) error {
	advNetwork := &advertisedNetwork{}

	lbServer.advertisedNetworksMu.Lock()
	if n, ok := lbServer.advertisedNetworks[netID]; !ok {
		lbServer.advertisedNetworks[netID] = advNetwork
	} else {
		advNetwork = n
	}
	lbServer.advertisedNetworksMu.Unlock()

	advNetwork.Lock()
	if slices.Contains(advNetwork.subnets, subnet) {
		advNetwork.Unlock()
		return ErrSubnetAlreadyAdvertised
	}
	// Reserve the subnet before the external call.
	advNetwork.subnets = append(advNetwork.subnets, subnet)
	advNetwork.Unlock()

	a, err := isPrefixAdvertised(ctx, subnet)
	if err != nil {
		advNetwork.Lock()
		// Remove the reserved subnet if failed to check.
		idx := slices.Index(advNetwork.subnets, subnet)
		if idx >= 0 {
			advNetwork.subnets = slices.Delete(advNetwork.subnets, idx, idx+1)
		}
		advNetwork.Unlock()
		return fmt.Errorf("check if subnet %s is advertised: %w", subnet, err)
	}
	if !a {
		if err := advertisePrefix(ctx, subnet); err != nil {
			advNetwork.Lock()
			// Remove the reserved subnet if advertising failed.
			idx := slices.Index(advNetwork.subnets, subnet)
			if idx >= 0 {
				advNetwork.subnets = slices.Delete(advNetwork.subnets, idx, idx+1)
			}
			advNetwork.Unlock()
			return fmt.Errorf("advertise the subnet %s: %w", subnet, err)
		}
	}

	return nil
}

func delAdvertisedNetwork(ctx context.Context, netID string) error {
	lbServer.advertisedNetworksMu.Lock()

	advNetwork, ok := lbServer.advertisedNetworks[netID]
	if !ok {
		lbServer.advertisedNetworksMu.Unlock()
		return fmt.Errorf("network %s is not advertised", netID)
	}
	lbServer.advertisedNetworksMu.Unlock()

	advNetwork.Lock()
	subnets := append([]string(nil), advNetwork.subnets...)
	advNetwork.Unlock()

	// A partial failure deliberately leaves the network in the map: a retry is
	// safe because [isPrefixAdvertised] reports already-withdrawn subnets as
	// not advertised, so they are skipped on the next attempt. All errors are
	// reported.
	var errs []string
	for _, subnet := range subnets {
		a, err := isPrefixAdvertised(ctx, subnet)
		if err != nil {
			errs = append(errs, fmt.Sprintf("check if subnet %s is advertised: %s", subnet, err))
			continue
		}
		if a {
			if err := withdrawPrefix(ctx, subnet); err != nil {
				errs = append(errs, fmt.Sprintf("withdraw the subnet %s: %s", subnet, err))
			}
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, ";"))
	}

	lbServer.advertisedNetworksMu.Lock()
	delete(lbServer.advertisedNetworks, netID)
	lbServer.advertisedNetworksMu.Unlock()
	return nil
}
