package main

import (
	"context"
	"net"
	"os"

	"github.com/davecgh/go-spew/spew"
	"github.com/olljanat/docker-bgp-lb/api"
	"github.com/sirupsen/logrus"
)

var driverScope = "local"
var lbServer *bgpLB
var log = &logrus.Logger{}
var scs = spew.ConfigState{Indent: "  "}
var stateFile = "/bgplb.json"

func main() {
	log = &logrus.Logger{
		Out:   os.Stdout,
		Level: logrus.DebugLevel,
		Formatter: &logrus.TextFormatter{
			FullTimestamp:          true,
			DisableLevelTruncation: true,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := initDockerClient(ctx); err != nil {
		log.Errorf("Failed to setup a Docker client: %v", err)
		return
	}
	defer dockerClient.Close()

	peerAddress := os.Getenv("PEER_ADDRESS")
	if peerAddress == "" {
		log.Error("Environment variable PEER_ADDRESS is required")
		return
	}

	if net.ParseIP(peerAddress) == nil {
		log.Errorf("Failed to parse PEER_ADDRESS=%s as IP address", peerAddress)
		return
	}

	if err := startBgpServer(peerAddress); err != nil {
		log.Errorf("Failed to start BGP server: %v", err)
		return
	}

	if os.Getenv("GLOBAL_SCOPE") == "true" {
		driverScope = "global"
	}

	log.Infof("Starting Docker BGP LB Plugin")

	lbServer = &bgpLB{
		advertisedNetworks: make(map[string]*advertisedNetwork),
		scope:              driverScope,
	}
	go advertiseNetworksOnStart(ctx)
	go watchDockerEvents(ctx)
	// Load saves networks configuration but only when we are not running in swarm mode.
	// This is because swarm will automatically create/remove networks when needed.
	lbServer.Lock()
	if driverScope == "global" {
		log.Info("Running in Swarm mode, starting with an empty configuration.")
		lbServer.Networks = make(map[string]*bgpNetwork)
	} else {
		d, err := loadState()
		if err != nil {
			log.Info("Failed to load data, starting with an empty configuration.")
			lbServer.Networks = make(map[string]*bgpNetwork)
		} else {
			lbServer.Networks = d.Networks
		}
	}

	for id, network := range lbServer.Networks {
		if err := createBridgeFromNetID(id); err != nil {
			log.Printf("Failed to create bridge for network %s: %v", id, err)
		}
		network.endpoints = make(map[string]*bgpLBEndpoint)
	}
	lbServer.Unlock()

	h := api.NewHandler(lbServer)
	if err := h.ServeUnix("bgplb", 0); err != nil {
		log.Errorf("ServeUnix failed: %v", err)
		return
	}
}
