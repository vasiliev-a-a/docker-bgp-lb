package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	apiGoBGP "github.com/osrg/gobgp/v3/api"
	loggerGoBGP "github.com/osrg/gobgp/v3/pkg/log"
	serverGoBGP "github.com/osrg/gobgp/v3/pkg/server"
	"github.com/vishvananda/netlink"
	apb "google.golang.org/protobuf/types/known/anypb"
)

var (
	bgpServer = serverGoBGP.BgpServer{}
	localAS   = uint32(0)
	routerID  = ""
)

func startBgpServer(peerAddress string) error {
	routerID = os.Getenv("ROUTER_ID")
	if routerID == "" || net.ParseIP(routerID) == nil {
		return fmt.Errorf("environment variable ROUTER_ID is required")
	}

	routerPortInt, err := strconv.Atoi(os.Getenv("ROUTER_PORT"))
	if err != nil {
		return fmt.Errorf("parse ROUTER_PORT=%s as integer: %w", os.Getenv("ROUTER_PORT"), err)
	}
	routerPort := int32(routerPortInt)

	localAsInt, err := strconv.Atoi(os.Getenv("LOCAL_AS"))
	if err != nil {
		return fmt.Errorf("parse LOCAL_AS=%s as integer: %w", os.Getenv("LOCAL_AS"), err)
	}
	localAS = uint32(localAsInt)

	peerAsInt, err := strconv.Atoi(os.Getenv("PEER_AS"))
	if err != nil {
		return fmt.Errorf("parse PEER_AS=%s as integer: %w", os.Getenv("PEER_AS"), err)
	}
	peerAs := uint32(peerAsInt)

	log.Infof("Starting BGP server")
	bgpLogger := loggerGoBGP.NewDefaultLogger()
	bgpServer = *serverGoBGP.NewBgpServer(serverGoBGP.LoggerOption(bgpLogger))
	go bgpServer.Serve()
	err = bgpServer.StartBgp(context.Background(), &apiGoBGP.StartBgpRequest{
		Global: &apiGoBGP.Global{
			RouterId:   routerID,
			Asn:        localAS,
			ListenPort: routerPort,
		},
	})
	if err != nil {
		return err
	}

	n := &apiGoBGP.Peer{
		Conf: &apiGoBGP.PeerConf{
			NeighborAddress: peerAddress,
			PeerAsn:         peerAs,
		},
	}
	peerPassword := os.Getenv("PEER_PASSWORD")
	if peerPassword != "" {
		n.Conf.AuthPassword = peerPassword
	}
	if err := bgpServer.AddPeer(context.Background(), &apiGoBGP.AddPeerRequest{
		Peer: n,
	}); err != nil {
		return err
	}

	return nil
}

func addRoute(ctx context.Context, NetworkID, EndpointID, ipv4, ipv6 string) {
	if ready := waitEndpointReady(ctx, NetworkID, EndpointID); ready == false {
		return
	}

	log := log.WithField("network.id", NetworkID).WithField("endpoint.id", EndpointID)

	bridgeName, err := getBridgeNameByNetID(NetworkID)
	if err != nil {
		log.Errorf("addRoute: failed to get bridge for network: %v", err)
		return
	}

	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		log.Errorf("addRoute: failed to get bridge interface for network: %v", err)
		return
	}

	// If installation of a local route fails, we return fast since we could not process incoming traffic.

	if ipv4 != "" {
		ip, ipv4Dst, err := net.ParseCIDR(ipv4)
		if err != nil {
			log.Errorf("addRoute: failed to parse %s as network prefix: %v", ipv4, err)
			return
		}

		if ip.String() != "0.0.0.0" {
			route := netlink.Route{Dst: ipv4Dst, LinkIndex: bridge.Attrs().Index}

			if err := netlink.RouteAdd(&route); err != nil && !errors.Is(err, os.ErrExist) {
				log.Errorf("addRoute: failed to install local route to %s: %v", ipv4Dst.String(), err)
				return
			}
			if errors.Is(err, os.ErrExist) {
				log.Infof("addRoute: local route to %s is already present", ipv4Dst.String())
			} else {
				log.Infof("addRoute: installed local route to %s", ipv4Dst.String())
			}

			if err := addBgpRoute(ip.String(), 32, apiGoBGP.Family_AFI_IP); err != nil {
				log.Errorf("addRoute: failed to install BGP route to %s: %v", ipv4Dst.String(), err)
				return
			}
			log.Infof("addRoute: installed BGP route to %s", ipv4Dst.String())
		}
	}

	if ipv6 != "" {
		ip, ipv6Dst, err := net.ParseCIDR(ipv6)
		if err != nil {
			log.Errorf("addRoute: failed to parse %s as network prefix: %v", ipv6, err)
			return
		}

		route := netlink.Route{Dst: ipv6Dst, LinkIndex: bridge.Attrs().Index}

		if err := netlink.RouteAdd(&route); err != nil && !errors.Is(err, os.ErrExist) {
			log.Errorf("addRoute: failed to install local route to %s: %v", ipv6Dst.String(), err)
			return
		}
		if errors.Is(err, os.ErrExist) {
			log.Infof("addRoute: local route to %s is already present", ipv6Dst.String())
		} else {
			log.Infof("addRoute: installed local route to %s", ipv6Dst.String())
		}

		if err := addBgpRoute(ip.String(), 128, apiGoBGP.Family_AFI_IP6); err != nil {
			log.Errorf("addRoute: failed to install BGP route to %s: %v", ipv6Dst.String(), err)
			return
		}
		log.Infof("addRoute: installed BGP route to %s", ipv6Dst.String())
	}
}

func addBgpRoute(prefix string, mask int, ipFamily apiGoBGP.Family_Afi) error {
	nlri, _ := apb.New(&apiGoBGP.IPAddressPrefix{
		Prefix:    prefix,
		PrefixLen: uint32(mask),
	})
	a1, _ := apb.New(&apiGoBGP.OriginAttribute{
		Origin: 0,
	})
	a2, _ := apb.New(&apiGoBGP.NextHopAttribute{
		NextHop: routerID,
	})
	a3, _ := apb.New(&apiGoBGP.AsPathAttribute{
		Segments: []*apiGoBGP.AsSegment{
			{
				Type: 2,
			},
		},
	})
	attrs := []*apb.Any{a1, a2, a3}
	_, err := bgpServer.AddPath(context.Background(), &apiGoBGP.AddPathRequest{
		Path: &apiGoBGP.Path{
			Family: &apiGoBGP.Family{Afi: ipFamily, Safi: apiGoBGP.Family_SAFI_UNICAST},
			Nlri:   nlri,
			Pattrs: attrs,
		},
	})
	return err
}

func delRoute(NetworkID, EndpointID string) {
	log := log.WithField("network.id", NetworkID).WithField("endpoint.id", EndpointID)

	bridgeName, err := getBridgeNameByNetID(NetworkID)
	if err != nil {
		log.Errorf("delRoute: failed to get bridge for network: %v", err)
		return
	}

	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		log.Errorf("delRoute: failed to get bridge interface for network: %v", err)
		return
	}

	// Deletion of routes is performed on a best effort base.
	// Since we do not return any error, we can not affect upstream routines.
	// We only log every failure.

	v4routes, err := netlink.RouteList(bridge, netlink.FAMILY_V4)
	if err != nil {
		log.Errorf("delRoute: failed to get local IPv4 routes: %v", err)
	}
	for _, v4route := range v4routes {
		v4dst := &v4route.Dst.IP
		if err := delBgpRoute(v4dst.String(), 32, apiGoBGP.Family_AFI_IP); err != nil {
			log.Errorf("delRoute: failed to remove BGP route to %s: %v", v4dst.String(), err)
		} else {
			log.Infof("delRoute: removed BGP route to %s", v4dst.String())
		}
		if err := netlink.RouteDel(&v4route); err != nil {
			log.Errorf("delRoute: failed to remove local route to %s: %v", v4dst.String(), err)
		} else {
			log.Infof("delRoute: removed local route to %s", v4dst.String())
		}
	}

	v6routes, err := netlink.RouteList(bridge, netlink.FAMILY_V6)
	if err != nil {
		log.Errorf("delRoute: failed to get local IPv6 routes: %v", err)
	}
	for _, v6route := range v6routes {
		v6dst := &v6route.Dst.IP
		if err := delBgpRoute(v6dst.String(), 128, apiGoBGP.Family_AFI_IP6); err != nil {
			log.Errorf("delRoute: failed to remove BGP route to %s: %v", v6dst.String(), err)
		} else {
			log.Infof("delRoute: removed BGP route to %s", v6dst.String())
		}
		if err := netlink.RouteDel(&v6route); err != nil {
			log.Errorf("delRoute: failed to remove local route to %s: %v", v6dst.String(), err)
		} else {
			log.Infof("delRoute: removed local route to %s", v6dst.String())
		}
	}
}

func delBgpRoute(prefix string, mask int, ipFamily apiGoBGP.Family_Afi) error {
	nlri, _ := apb.New(&apiGoBGP.IPAddressPrefix{
		Prefix:    prefix,
		PrefixLen: uint32(mask),
	})
	a1, _ := apb.New(&apiGoBGP.OriginAttribute{
		Origin: 0,
	})
	a2, _ := apb.New(&apiGoBGP.NextHopAttribute{
		NextHop: routerID,
	})
	a3, _ := apb.New(&apiGoBGP.AsPathAttribute{
		Segments: []*apiGoBGP.AsSegment{
			{
				Type: 2,
			},
		},
	})
	attrs := []*apb.Any{a1, a2, a3}
	p1 := &apiGoBGP.Path{
		Family: &apiGoBGP.Family{Afi: ipFamily, Safi: apiGoBGP.Family_SAFI_UNICAST},
		Nlri:   nlri,
		Pattrs: attrs,
	}
	err := bgpServer.DeletePath(context.Background(), &apiGoBGP.DeletePathRequest{
		TableType: apiGoBGP.TableType_GLOBAL,
		Path:      p1,
	})
	if err != nil {
		return err
	}
	return nil
}

func isPrefixAdvertised(ctx context.Context, prefix string) (bool, error) {
	_, ipnet, err := net.ParseCIDR(prefix)
	if err != nil {
		return false, fmt.Errorf("parse %s as network prefix: %w", prefix, err)
	}

	var counter int
	callback := func(*apiGoBGP.Destination) { counter++ }
	request := &apiGoBGP.ListPathRequest{
		Family:   &apiGoBGP.Family{Afi: apiGoBGP.Family_AFI_IP, Safi: apiGoBGP.Family_SAFI_UNICAST},
		Prefixes: []*apiGoBGP.TableLookupPrefix{{Prefix: prefix}},
	}

	if ipnet.IP.To4() == nil && strings.Contains(ipnet.IP.String(), ":") {
		request.Family.Afi = apiGoBGP.Family_AFI_IP6
	}

	if err := bgpServer.ListPath(ctx, request, callback); err != nil {
		return false, fmt.Errorf("lookup BGP paths: %w", err)
	}

	return counter > 0, nil
}

func advertisePrefix(ctx context.Context, prefix string) error {
	_, ipnet, err := net.ParseCIDR(prefix)
	if err != nil {
		return fmt.Errorf("parse %s as network prefix: %w", prefix, err)
	}

	mask, _ := ipnet.Mask.Size()
	var family apiGoBGP.Family_Afi
	if ipnet.IP.To4() == nil && strings.Contains(ipnet.IP.String(), ":") {
		family = apiGoBGP.Family_AFI_IP6
	} else {
		family = apiGoBGP.Family_AFI_IP
	}

	nlri, _ := apb.New(&apiGoBGP.IPAddressPrefix{
		Prefix:    ipnet.IP.String(),
		PrefixLen: uint32(mask),
	})
	a1, _ := apb.New(&apiGoBGP.OriginAttribute{
		Origin: 0,
	})
	a2, _ := apb.New(&apiGoBGP.NextHopAttribute{
		NextHop: routerID,
	})
	a3, _ := apb.New(&apiGoBGP.AsPathAttribute{
		Segments: []*apiGoBGP.AsSegment{
			{
				Type: 2,
			},
		},
	})
	attrs := []*apb.Any{a1, a2, a3}
	if _, err := bgpServer.AddPath(ctx, &apiGoBGP.AddPathRequest{
		Path: &apiGoBGP.Path{
			Family: &apiGoBGP.Family{Afi: family, Safi: apiGoBGP.Family_SAFI_UNICAST},
			Nlri:   nlri,
			Pattrs: attrs,
		},
	}); err != nil {
		return fmt.Errorf("add BGP path: %w", err)
	}

	return nil
}

func withdrawPrefix(ctx context.Context, prefix string) error {
	_, ipnet, err := net.ParseCIDR(prefix)
	if err != nil {
		return fmt.Errorf("parse %s as network prefix: %w", prefix, err)
	}

	mask, _ := ipnet.Mask.Size()
	var family apiGoBGP.Family_Afi
	if ipnet.IP.To4() == nil && strings.Contains(ipnet.IP.String(), ":") {
		family = apiGoBGP.Family_AFI_IP6
	} else {
		family = apiGoBGP.Family_AFI_IP
	}

	nlri, _ := apb.New(&apiGoBGP.IPAddressPrefix{
		Prefix:    ipnet.IP.String(),
		PrefixLen: uint32(mask),
	})
	a1, _ := apb.New(&apiGoBGP.OriginAttribute{
		Origin: 0,
	})
	a2, _ := apb.New(&apiGoBGP.NextHopAttribute{
		NextHop: routerID,
	})
	a3, _ := apb.New(&apiGoBGP.AsPathAttribute{
		Segments: []*apiGoBGP.AsSegment{
			{
				Type: 2,
			},
		},
	})
	attrs := []*apb.Any{a1, a2, a3}
	p1 := &apiGoBGP.Path{
		Family: &apiGoBGP.Family{Afi: family, Safi: apiGoBGP.Family_SAFI_UNICAST},
		Nlri:   nlri,
		Pattrs: attrs,
	}
	if err := bgpServer.DeletePath(ctx, &apiGoBGP.DeletePathRequest{
		TableType: apiGoBGP.TableType_GLOBAL,
		Path:      p1,
	}); err != nil {
		return fmt.Errorf("delete BGP path: %w", err)
	}

	return nil
}
