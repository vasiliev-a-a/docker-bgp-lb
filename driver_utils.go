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
	b.scope = driverScope
	return &b, nil
}

func addAdvertisedSubnet(ctx context.Context, netID, subnet string) error {
	net := &advertisedNetwork{}

	lbServer.Lock()
	if n, ok := lbServer.advertisedNetworks[netID]; !ok {
		lbServer.advertisedNetworks[netID] = net
	} else {
		net = n
	}
	lbServer.Unlock()

	net.Lock()
	if slices.Contains(net.subnets, subnet) {
		net.Unlock()
		return fmt.Errorf("addAdvertisedSubnet: the subnet '%s' is already advertised", subnet)
	}
	// reserve the subnet before the external call
	net.subnets = append(net.subnets, subnet)
	net.Unlock()

	if !isPrefixAdvertised(ctx, subnet) {
		if err := advertisePrefix(ctx, subnet); err != nil {
			net.Lock()
			// remove the reserved subnet if advertising failed
			idx := slices.Index(net.subnets, subnet)
			if idx >= 0 {
				net.subnets = slices.Delete(net.subnets, idx, idx+1)
			}
			net.Unlock()
			return fmt.Errorf("addAdvertisedSubnet: failed to advertise the subnet: %w", err)
		}
	}

	return nil
}

func delAdvertisedNetwork(ctx context.Context, netID string) error {
	lbServer.Lock()

	net, ok := lbServer.advertisedNetworks[netID]
	if !ok {
		lbServer.Unlock()
		return fmt.Errorf("delAdvertisedNetwork: network '%s' is not advertised", netID[:11])
	}
	lbServer.Unlock()

	net.Lock()
	subnets := append([]string(nil), net.subnets...)
	net.Unlock()

	for _, subnet := range subnets {
		if isPrefixAdvertised(ctx, subnet) {
			if err := withdrawPrefix(ctx, subnet); err != nil {
				return fmt.Errorf("delAdvertisedNetwork: failed to withdraw the subnet %w", err)
			}
		}
	}

	lbServer.Lock()
	delete(lbServer.advertisedNetworks, netID)
	lbServer.Unlock()
	return nil
}
