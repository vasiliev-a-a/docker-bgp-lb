package main

import (
	"sync"
	"testing"
)

// The tests in this file exercise the lock hand-off protocol used by the
// network/endpoint API handlers (lock networksMu -> lock per-network mutex -> release networksMu)
// and the [bgpLB.isEndpointManaged] liveness check used by the background loops.
// They only touch the in-memory maps, so they run unprivileged, without
// netlink or a Docker client.

func TestIsEndpointManaged_Semantics(t *testing.T) {
	d := &bgpLB{Networks: map[string]*bgpNetwork{}}
	d.Networks["net1"] = &bgpNetwork{endpoints: map[string]*bgpLBEndpoint{}}
	d.Networks["net1"].endpoints["ep1"] = &bgpLBEndpoint{}

	cases := []struct {
		name        string
		networkID   string
		endpointID  string
		wantManaged bool
	}{
		{"managed endpoint", "net1", "ep1", true},
		{"unknown network", "net2", "ep1", false},
		{"unknown endpoint", "net1", "ep2", false},
		{"both unknown", "net2", "ep2", false},
	}

	for _, c := range cases {
		if got := d.isEndpointManaged(c.networkID, c.endpointID); got != c.wantManaged {
			t.Errorf("%s: isEndpointManaged(%q, %q) = %v, want %v",
				c.name, c.networkID, c.endpointID, got, c.wantManaged)
		}
	}

	// The check must turn false once the endpoint is deleted ([bgpLB.DeleteEndpoint])
	// or its network is deleted ([bgpLB.DeleteNetwork]).
	d.Networks["net1"].Lock()
	delete(d.Networks["net1"].endpoints, "ep1")
	d.Networks["net1"].Unlock()
	if d.isEndpointManaged("net1", "ep1") {
		t.Error("endpoint reported as managed after DeleteEndpoint")
	}

	d.Networks["net1"].endpoints["ep1"] = &bgpLBEndpoint{}
	d.networksMu.Lock()
	delete(d.Networks, "net1")
	d.networksMu.Unlock()
	if d.isEndpointManaged("net1", "ep1") {
		t.Error("endpoint reported as managed after DeleteNetwork")
	}
}

// [insertEndpoint] and [removeEndpoint] mutate the endpoint map using the same
// lock protocol as the [bgpLB.CreateEndpoint]/[bgpLB.DeleteEndpoint] handlers.
// The handlers themselves cannot be used here: [bgpLB.CreateEndpoint] spawns [addRoute],
// which requires an initialized Docker client.
func insertEndpoint(d *bgpLB, networkID, endpointID string) {
	d.networksMu.Lock()
	network := d.Networks[networkID]
	network.Lock()
	d.networksMu.Unlock()
	network.endpoints[endpointID] = &bgpLBEndpoint{}
	network.Unlock()
}

func removeEndpoint(d *bgpLB, networkID, endpointID string) {
	d.networksMu.Lock()
	network := d.Networks[networkID]
	network.Lock()
	d.networksMu.Unlock()
	delete(network.endpoints, endpointID)
	network.Unlock()
}

// The churn tests have no assertions on individual [bgpLB.isEndpointManaged] results:
// they are nondeterministic, depending on how the reader interleaves with the churn.
// Data races are reported by the race detector when run with `-race`;
// unsynchronized map access crashes the runtime even without it,
// and a lock-ordering deadlock fails the test timeout.

// The [waitEndpointReady] background loop calls [bgpLB.isEndpointManaged] concurrently
// with endpoint creations and deletions.
func TestIsEndpointManaged_ConcurrentEndpointChurn(t *testing.T) {
	d := &bgpLB{Networks: map[string]*bgpNetwork{}}
	d.Networks["net1"] = &bgpNetwork{endpoints: map[string]*bgpLBEndpoint{}}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() { // reader: the background-loop liveness check
		defer wg.Done()
		for i := 0; i < 200000; i++ {
			_ = d.isEndpointManaged("net1", "ep1")
		}
	}()

	go func() { // writer: endpoint create/delete churn
		defer wg.Done()
		for i := 0; i < 50000; i++ {
			if i%2 == 0 {
				insertEndpoint(d, "net1", "ep1")
			} else {
				removeEndpoint(d, "net1", "ep1")
			}
		}
	}()

	wg.Wait()
}

// The background loops also run while networks are created and deleted, which
// mutates the [bgpLB.Networks] map itself. This mirrors the map mutations of
// [bgpLB.CreateNetwork]/[bgpLB.DeleteNetwork] (their netlink calls are skipped: they are
// irrelevant for the map access protocol), including deletion and re-creation
// of the very network being read.
func TestIsEndpointManaged_ConcurrentNetworkChurn(t *testing.T) {
	d := &bgpLB{Networks: map[string]*bgpNetwork{}}
	network := &bgpNetwork{endpoints: map[string]*bgpLBEndpoint{}}
	network.endpoints["ep1"] = &bgpLBEndpoint{}
	d.Networks["net1"] = network

	var wg sync.WaitGroup
	wg.Add(2)

	go func() { // reader: the background-loop liveness check
		defer wg.Done()
		for i := 0; i < 200000; i++ {
			_ = d.isEndpointManaged("net1", "ep1")
		}
	}()

	go func() { // writer: create/delete churn of unrelated and read networks
		defer wg.Done()
		for i := 0; i < 50000; i++ {
			d.networksMu.Lock()
			if i%2 == 0 {
				// [bgpLB.DeleteNetwork] of the network being read, plus
				// create/delete of an unrelated network.
				delete(d.Networks, "net1")
				d.Networks["churn"] = &bgpNetwork{endpoints: map[string]*bgpLBEndpoint{}}
				delete(d.Networks, "churn")
			} else {
				// [bgpLB.CreateNetwork] re-creating the read network. This is a separate
				// API call from the deletion above, hence a separate critical
				// section: the reader can observe the network-gone window between
				// the two, exactly as with real handler calls.
				d.Networks["net1"] = network
				d.Networks["churn"] = &bgpNetwork{endpoints: map[string]*bgpLBEndpoint{}}
				delete(d.Networks, "churn")
			}
			d.networksMu.Unlock()
		}
	}()

	wg.Wait()
}
