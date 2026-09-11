package main

import (
	"context"
	"os"
	"sync"

	"github.com/olljanat/docker-bgp-lb/api"
)

type bgpLBEndpoint struct {
	endpointInterface *api.EndpointInterface
	vethInside        string
	vethOutside       string
}

type bgpNetwork struct {
	endpoints map[string]*bgpLBEndpoint
	sync.Mutex
}

type advertisedNetwork struct {
	subnets []string
	sync.Mutex
}

type bgpLB struct {
	Networks   map[string]*bgpNetwork
	networksMu sync.Mutex

	advertisedNetworks   map[string]*advertisedNetwork
	advertisedNetworksMu sync.Mutex
	// ctx is the plugin's lifetime context,
	// used for background route operations.
	ctx   context.Context
	scope string
}

func initLBServer(ctx context.Context) *bgpLB {
	lb := &bgpLB{
		advertisedNetworks: make(map[string]*advertisedNetwork),
		ctx:                ctx,
	}

	if os.Getenv("GLOBAL_SCOPE") == "true" {
		lb.scope = "global"
	} else {
		lb.scope = "local"
	}

	// Load saved network configuration only when we are not running in Swarm mode.
	// This is because Swarm will automatically create/remove networks when needed.
	if lb.scope == "global" {
		log.Info("Running in Swarm mode. Starting with an empty configuration.")
		lb.Networks = make(map[string]*bgpNetwork)
	} else {
		b, err := loadState()
		if err != nil {
			log.Warnf("Did not load the state: %v. Starting with an empty configuration.", err)
			lb.Networks = make(map[string]*bgpNetwork)
		} else {
			lb.Networks = b.Networks
		}
	}

	for i, n := range lb.Networks {
		if err := createBridgeFromNetID(i); err != nil {
			log.Warnf("Failed to create bridge for network %s: %v", i, err)
		}
		n.endpoints = make(map[string]*bgpLBEndpoint)
	}

	return lb
}
