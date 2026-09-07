package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
)

func (lb *bgpLB) saveState() error {
	data, err := json.Marshal(lb)
	if err != nil {
		return err
	}
	return os.WriteFile(stateFile, data, 0644)
}

func loadState() (*bgpLB, error) {
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return nil, err
	}
	var b bgpLB
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}

	// Small guard if persisted state is corrupted, contains {"Networks": null}, or the field is absent.
	if b.Networks == nil {
		b.Networks = make(map[string]*bgpNetwork)
	}

	b.scope = driverScope
	return &b, nil
}

func addAdvertisedSubnet(ctx context.Context, netID, subnet string) error {
	advNetwork := &advertisedNetwork{}

	lbServer.Lock()
	if n, ok := lbServer.advertisedNetworks[netID]; !ok {
		lbServer.advertisedNetworks[netID] = advNetwork
	} else {
		advNetwork = n
	}
	lbServer.Unlock()

	advNetwork.Lock()
	if slices.Contains(advNetwork.subnets, subnet) {
		advNetwork.Unlock()
		return fmt.Errorf("addAdvertisedSubnet: the subnet '%s' is already advertised", subnet)
	}
	// Reserve the subnet before the external call.
	advNetwork.subnets = append(advNetwork.subnets, subnet)
	advNetwork.Unlock()

	if !isPrefixAdvertised(ctx, subnet) {
		if err := advertisePrefix(ctx, subnet); err != nil {
			advNetwork.Lock()
			// Remove the reserved subnet if advertising failed.
			idx := slices.Index(advNetwork.subnets, subnet)
			if idx >= 0 {
				advNetwork.subnets = slices.Delete(advNetwork.subnets, idx, idx+1)
			}
			advNetwork.Unlock()
			return fmt.Errorf("addAdvertisedSubnet: failed to advertise the subnet: %w", err)
		}
	}

	return nil
}

func delAdvertisedNetwork(ctx context.Context, netID string) error {
	lbServer.Lock()

	advNetwork, ok := lbServer.advertisedNetworks[netID]
	if !ok {
		lbServer.Unlock()
		return fmt.Errorf("network %s is not advertised", netID)
	}
	lbServer.Unlock()

	advNetwork.Lock()
	subnets := append([]string(nil), advNetwork.subnets...)
	advNetwork.Unlock()

	for _, subnet := range subnets {
		if isPrefixAdvertised(ctx, subnet) {
			if err := withdrawPrefix(ctx, subnet); err != nil {
				return fmt.Errorf("withdraw subnet %s: %w", subnet, err)
			}
		}
	}

	lbServer.Lock()
	delete(lbServer.advertisedNetworks, netID)
	lbServer.Unlock()
	return nil
}
