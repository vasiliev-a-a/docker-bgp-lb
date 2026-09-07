package main

import (
	"fmt"
	"strings"

	"github.com/docker/docker/libnetwork/ns"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

const (
	bridgeNameLen    = 9
	bridgeNamePrefix = "bgplb"
)

func getBridgeNameByNetID(netID string) (string, error) {
	if netID == "" {
		return "", fmt.Errorf("network ID cannot be empty")
	}

	return bridgeNamePrefix + "-" + truncate(netID, bridgeNameLen), nil
}

func createBridgeFromNetID(netID string) error {
	bridgeName, err := getBridgeNameByNetID(netID)
	if err != nil {
		return fmt.Errorf("get bridge name: %w", err)
	}

	exists, err := bridgeInterfaceExists(bridgeName)
	if err != nil {
		return fmt.Errorf("check bridge %s exist: %w", bridgeName, err)
	}

	if !exists {
		linkAttrs := netlink.NewLinkAttrs()
		linkAttrs.Name = bridgeName

		if err := netlink.LinkAdd(&netlink.Bridge{
			LinkAttrs: linkAttrs,
		}); err != nil {
			return fmt.Errorf("create bridge %s: %w", bridgeName, err)
		}
	}

	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("get bridge interface %s: %w", bridgeName, err)
	}

	if err := patchBridge(bridge); err != nil {
		return fmt.Errorf("adjust bridge %s: %w", bridgeName, err)
	}

	return nil
}

func patchBridge(bridge netlink.Link) error {
	// Creates a new RTM_NEWLINK request
	// NLM_F_ACK is used to receive acks when operations are executed
	req := nl.NewNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)

	// Search for the bridge interface by its index (and bring it UP too)
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Change = unix.IFF_UP
	msg.Flags = unix.IFF_UP
	msg.Index = int32(bridge.Attrs().Index)
	req.AddData(msg)

	// Patch ageing_time and group_fwd_mask
	linkInfo := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	linkInfo.AddRtAttr(nl.IFLA_INFO_KIND, nl.NonZeroTerminated(bridge.Type()))

	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	data.AddRtAttr(nl.IFLA_BR_AGEING_TIME, nl.Uint32Attr(0))
	data.AddRtAttr(nl.IFLA_BR_GROUP_FWD_MASK, nl.Uint16Attr(0xfff8))

	req.AddData(linkInfo)

	// Execute the request. NETLINK_ROUTE is used to send link updates.
	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	if err != nil {
		return fmt.Errorf("execute netlink request: %w", err)
	}

	return nil
}

func deleteBridge(netID string) error {
	bridgeName, err := getBridgeNameByNetID(netID)
	if err != nil {
		return fmt.Errorf("get bridge name: %w", err)
	}

	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("get bridge interface %s: %w", bridgeName, err)
	}

	if err := netlink.LinkDel(bridge); err != nil {
		return fmt.Errorf("delete bridge interface %s: %w", bridgeName, err)
	}

	return nil
}

func attachInterfaceToBridge(bridgeName string, interfaceName string) error {
	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("get bridge interface %s: %w", bridgeName, err)
	}

	iface, err := netlink.LinkByName(interfaceName)
	if err != nil {
		return fmt.Errorf("get interface %s: %w", interfaceName, err)
	}

	if err := netlink.LinkSetMaster(iface, bridge); err != nil {
		return fmt.Errorf("set interface %s to master of bridge %s: %w", interfaceName, bridgeName, err)
	}
	if err := netlink.LinkSetUp(iface); err != nil {
		return fmt.Errorf("start interface %s: %w", interfaceName, err)
	}

	return nil
}

func bridgeInterfaceExists(name string) (bool, error) {
	nlh := ns.NlHandle()
	link, err := nlh.LinkByName(name)

	if err != nil {
		if strings.Contains(err.Error(), "Link not found") {
			return false, nil
		}

		return false, fmt.Errorf("get interface %s: %w", name, err)
	}

	if link.Type() == "bridge" {
		return true, nil
	}

	return false, fmt.Errorf("interface %s is not a bridge", name)
}
