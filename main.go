package main

import (
	"context"
	"net"
	"os"

	"github.com/davecgh/go-spew/spew"
	"github.com/olljanat/docker-bgp-lb/api"
	"github.com/sirupsen/logrus"
)

var lbServer *bgpLB
var log = &logrus.Logger{}
var scs = spew.ConfigState{Indent: "  "}
var stateFile = "/bgplb.json"

func main() {
	// [os.Exit] skips all other deferred calls,
	// so this defer — registered first and therefore run last,
	// after `cancel()` and `dockerClient.Close()` have completed.
	// On the success path `exitCode` stays 0 and [main] returns normally.
	exitCode := 0
	defer func() {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}()

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
		exitCode = 1
		log.Errorf("Failed to setup a Docker client: %v", err)
		return
	}
	defer dockerClient.Close()

	peerAddress := os.Getenv("PEER_ADDRESS")
	if peerAddress == "" {
		exitCode = 1
		log.Error("Environment variable PEER_ADDRESS is required")
		return
	}

	if net.ParseIP(peerAddress) == nil {
		exitCode = 1
		log.Errorf("Failed to parse PEER_ADDRESS=%s as IP address", peerAddress)
		return
	}

	if err := startBgpServer(peerAddress); err != nil {
		exitCode = 1
		log.Errorf("Failed to start BGP server: %v", err)
		return
	}

	log.Infof("Starting Docker BGP LB Plugin")

	lbServer = initLBServer(ctx)
	go advertiseNetworksOnStart(ctx)
	go watchDockerEvents(ctx)

	h := api.NewHandler(lbServer)
	if err := h.ServeUnix("bgplb", 0); err != nil {
		exitCode = 1
		log.Errorf("ServeUnix failed: %v", err)
		return
	}
}
