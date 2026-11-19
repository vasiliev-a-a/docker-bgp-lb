package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/davecgh/go-spew/spew"
	"github.com/olljanat/docker-bgp-lb/api"
	"github.com/sirupsen/logrus"
)

var lbServer *bgpLB
var log = &logrus.Logger{}
var scs = spew.ConfigState{Indent: "  "}
var stateFile = "/bgplb.json"
var handledSignals = []os.Signal{syscall.SIGTERM, syscall.SIGINT}

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

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	// Spinoff the signal handling gouroutine.
	go func(signals []os.Signal) {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, signals...)
		defer signal.Stop(sigCh)

		select {
		case <-ctx.Done():
			// The main's context is canceled in another path.
		case s := <-sigCh:
			switch s {
			case syscall.SIGINT, syscall.SIGTERM:
				cancel(fmt.Errorf("%q signal is received", s.String()))
			}
		}
	}(handledSignals)

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
	errCh := make(chan error, 1)

	go func() {
		errCh <- h.ServeUnix("bgplb", 0)
	}()

	select {
	case <-ctx.Done():
		log.Warnf("Plugin is stopping due to: %v", context.Cause(ctx))
	case err := <-errCh:
		if err != nil {
			exitCode = 1
			log.Errorf("ServeUnix failed: %v", err)
		}
	}
}
