package main

import (
	"errors"
	"net"

	"github.com/docker/docker/libnetwork/types"
	"github.com/olljanat/docker-bgp-lb/api"
)

func (d *bgpLB) GetIpamCapabilities() (*api.CapabilitiesResponse, error) {
	return &api.CapabilitiesResponse{RequiresMACAddress: true}, nil
}

func (d *bgpLB) GetNetCapabilities() (*api.CapabilitiesResponse, error) {
	capabilities := &api.CapabilitiesResponse{
		Scope: d.scope,
	}

	return capabilities, nil
}

func (d *bgpLB) GetDefaultAddressSpaces() (*api.AddressSpacesResponse, error) {
	return &api.AddressSpacesResponse{LocalDefaultAddressSpace: api.GlobalScope,
		GlobalDefaultAddressSpace: api.GlobalScope}, nil
}

func (d *bgpLB) RequestPool(r *api.RequestPoolRequest) (*api.RequestPoolResponse, error) {
	pool := ""
	if r.V6 {
		if r.Options["v6subnet"] == "" {
			return &api.RequestPoolResponse{}, errors.New("IPv6 subnet is required")
		}
		pool = r.Options["v6subnet"]
	} else {
		if r.Pool == "" {
			return &api.RequestPoolResponse{PoolID: "0.0.0.0/32", Pool: "0.0.0.0/32"}, nil
		}
		pool = r.Pool
	}

	_, ipnet, err := net.ParseCIDR(pool)
	if err != nil {
		return &api.RequestPoolResponse{}, err
	}
	mask, _ := ipnet.Mask.Size()
	if !r.V6 && mask != 32 {
		return &api.RequestPoolResponse{}, errors.New("only subnet mask /32 is supported")
	}
	if r.V6 && mask != 128 {
		return &api.RequestPoolResponse{}, errors.New("only subnet mask /128 is supported")
	}

	return &api.RequestPoolResponse{PoolID: pool, Pool: pool}, nil
}

func (d *bgpLB) RequestAddress(r *api.RequestAddressRequest) (*api.RequestAddressResponse, error) {
	if r.Options["RequestAddressType"] == "com.docker.network.gateway" {
		return &api.RequestAddressResponse{Address: r.PoolID}, nil
	}

	return &api.RequestAddressResponse{Address: r.PoolID}, nil
}

func (d *bgpLB) ReleaseAddress(r *api.ReleaseAddressRequest) error {
	return nil
}

func (d *bgpLB) ReleasePool(r *api.ReleasePoolRequest) error {
	return nil
}

func (d *bgpLB) CreateNetwork(r *api.CreateNetworkRequest) error {
	d.Lock()
	defer d.Unlock()

	if _, ok := d.Networks[r.NetworkID]; ok {
		return types.ForbiddenErrorf("network %s exists", r.NetworkID)
	}

	err := createBridgeFromNetID(r.NetworkID)
	if err != nil {
		return err
	}

	bgpNetwork := &bgpNetwork{
		endpoints: make(map[string]*bgpLBEndpoint),
	}

	d.Networks[r.NetworkID] = bgpNetwork
	err = d.saveState()
	if err != nil {
		return err
	}

	return nil
}

func (d *bgpLB) DeleteNetwork(r *api.DeleteNetworkRequest) error {
	d.Lock()
	defer d.Unlock()

	/* Skip if not in map */
	if _, ok := d.Networks[r.NetworkID]; !ok {
		return nil
	}

	err := deleteBridge(r.NetworkID)
	if err != nil {
		return err
	}

	delete(d.Networks, r.NetworkID)
	err = d.saveState()
	if err != nil {
		return err
	}

	return nil
}

func (d *bgpLB) AllocateNetwork(r *api.AllocateNetworkRequest) (*api.AllocateNetworkResponse, error) {
	return nil, nil
}

func (d *bgpLB) FreeNetwork(r *api.FreeNetworkRequest) error {
	return nil
}

func (d *bgpLB) CreateEndpoint(r *api.CreateEndpointRequest) (*api.CreateEndpointResponse, error) {
	d.Lock()
	defer d.Unlock()

	/* Throw error if not in map */
	if _, ok := d.Networks[r.NetworkID]; !ok {
		return nil, types.ForbiddenErrorf("%s network does not exist", r.NetworkID)
	}

	d.Networks[r.NetworkID].endpoints[r.EndpointID] = &bgpLBEndpoint{}

	resp := &api.CreateEndpointResponse{}

	// Start Goroutine which will add local and BGP routes after container is up and running
	go addRoute(r.NetworkID, r.EndpointID, r.Interface.Address, r.Interface.AddressIPv6)

	return resp, nil
}

func (d *bgpLB) DeleteEndpoint(r *api.DeleteEndpointRequest) error {
	d.Lock()
	defer d.Unlock()

	/* Skip if not in map (both network and endpoint) */
	if _, netOk := d.Networks[r.NetworkID]; !netOk {
		return nil
	}

	if _, epOk := d.Networks[r.NetworkID].endpoints[r.EndpointID]; !epOk {
		return nil
	}

	delete(d.Networks[r.NetworkID].endpoints, r.EndpointID)

	return nil
}

func (d *bgpLB) EndpointInfo(r *api.InfoRequest) (*api.InfoResponse, error) {
	d.Lock()
	defer d.Unlock()

	/* Throw error (both network and endpoint) */
	if _, netOk := d.Networks[r.NetworkID]; !netOk {
		return nil, types.ForbiddenErrorf("%s network does not exist", r.NetworkID)
	}

	if _, epOk := d.Networks[r.NetworkID].endpoints[r.EndpointID]; !epOk {
		return nil, types.ForbiddenErrorf("%s endpoint does not exist", r.NetworkID)
	}

	endpointInfo := d.Networks[r.NetworkID].endpoints[r.EndpointID]
	value := make(map[string]string)

	value["ip_address"] = ""
	value["mac_address"] = ""
	value["veth_outside"] = endpointInfo.vethOutside

	resp := &api.InfoResponse{
		Value: value,
	}

	return resp, nil
}

func (d *bgpLB) Join(r *api.JoinRequest) (*api.JoinResponse, error) {
	d.Lock()
	defer d.Unlock()

	/* Throw error (both network and endpoint) */
	if _, netOk := d.Networks[r.NetworkID]; !netOk {
		return nil, types.ForbiddenErrorf("%s network does not exist", r.NetworkID)
	}

	if _, epOk := d.Networks[r.NetworkID].endpoints[r.EndpointID]; !epOk {
		return nil, types.ForbiddenErrorf("%s endpoint does not exist", r.NetworkID)
	}

	vethInside, vethOutside, err := createVethPair()
	if err != nil {
		return nil, err
	}

	if err := attachInterfaceToBridge(getBridgeNameByNetID(r.NetworkID), vethOutside); err != nil {
		return nil, err
	}

	d.Networks[r.NetworkID].endpoints[r.EndpointID].vethInside = vethInside
	d.Networks[r.NetworkID].endpoints[r.EndpointID].vethOutside = vethOutside

	resp := &api.JoinResponse{
		InterfaceName: api.InterfaceName{
			SrcName:   vethInside,
			DstPrefix: "eth",
		},
	}

	return resp, nil
}

func (d *bgpLB) Leave(r *api.LeaveRequest) error {
	d.Lock()
	defer d.Unlock()

	/* Throw error (both network and endpoint) */
	if _, netOk := d.Networks[r.NetworkID]; !netOk {
		return types.ForbiddenErrorf("%s network does not exist", r.NetworkID)
	}

	if _, epOk := d.Networks[r.NetworkID].endpoints[r.EndpointID]; !epOk {
		return types.ForbiddenErrorf("%s endpoint does not exist", r.NetworkID)
	}

	delRoute(r.NetworkID, r.EndpointID)

	endpointInfo := d.Networks[r.NetworkID].endpoints[r.EndpointID]

	if err := deleteVethPair(endpointInfo.vethOutside); err != nil {
		return err
	}

	return nil
}

func (d *bgpLB) DiscoverNew(r *api.DiscoveryNotification) error {
	return nil
}

func (d *bgpLB) DiscoverDelete(r *api.DiscoveryNotification) error {
	return nil
}

func (d *bgpLB) ProgramExternalConnectivity(r *api.ProgramExternalConnectivityRequest) error {
	return nil
}

func (d *bgpLB) RevokeExternalConnectivity(r *api.RevokeExternalConnectivityRequest) error {
	return nil
}
