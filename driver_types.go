package main

import "sync"

type bgpLBEndpoint struct {
	vethInside  string
	vethOutside string
}

type bgpNetwork struct {
	endpoints map[string]*bgpLBEndpoint
}

type advertisedNetwork struct {
	subnets []string
	sync.Mutex
}

type bgpLB struct {
	Networks map[string]*bgpNetwork

	advertisedNetworks map[string]*advertisedNetwork
	scope              string
	sync.Mutex
}
