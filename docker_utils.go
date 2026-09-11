package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

const (
	dockerDriverName = "ollijanatuinen/docker-bgp-lb:v1.8"
	SIGUSR2Number    = "12"
)

var (
	SIGUSR2Action  string
	SIGUSR2Enabled bool

	dockerClient *client.Client
)

func initDockerClient(ctx context.Context) error {
	c, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return err
	}
	c.NegotiateAPIVersion(ctx)

	dockerClient = c
	return nil
}

func advertiseNetworksOnStart(ctx context.Context) {
	// Set up backoff configuration and initial delay.
	backoffConfig := newBackoff(1*time.Second, 1.5, 0, 10*time.Second)
	var delay time.Duration // zero: the first attempt is immediate.

	for {
		select {
		case <-ctx.Done():
			log.Warnf("advertiseNetworksOnStart: operation is canceled: %v", context.Cause(ctx))
			return

		case <-time.After(delay):
			networkFilter := filters.NewArgs()
			networkFilter.Add("label", "bgplb_advertise=true")
			options := types.NetworkListOptions{
				Filters: networkFilter,
			}

			dockerNetworks, err := dockerClient.NetworkList(ctx, options)
			if err != nil {
				delay = backoffConfig.NextBackOff()
				log.Warnf("advertiseNetworksOnStart: failed to get networks from Docker: %v. Retrying in %s", err, delay)
				continue
			}

			for _, dockerNetwork := range dockerNetworks {
				log := log.WithField("network.id", dockerNetwork.ID).WithField("network.name", dockerNetwork.Name)
				ipamConfigs := dockerNetwork.IPAM.Config
				for _, ipam := range ipamConfigs {
					if err := addAdvertisedSubnet(ctx, dockerNetwork.ID, ipam.Subnet); err == nil {
						log.WithField("subnet", ipam.Subnet).Info("advertiseNetworksOnStart: advertising the subnet")
					} else {
						log.WithField("subnet", ipam.Subnet).Errorf("advertiseNetworksOnStart: failed to advertise the subnet: %v", err)
					}
				}
			}

			return
		}
	}
}

func getContainerByEndpointID(ctx context.Context, networkID, endpointID string) (string, error) {
	dockerNetwork, err := dockerClient.NetworkInspect(ctx, networkID, types.NetworkInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspect network %s: %w", networkID, err)
	}

	if len(dockerNetwork.Containers) == 0 {
		return "", fmt.Errorf("no containers found on network %s", networkID)
	}

	for containerID, endpoint := range dockerNetwork.Containers {
		if endpoint.EndpointID == endpointID {
			return containerID, nil
		}
	}

	return "", fmt.Errorf("endpoint %s not found on network %s", endpointID, networkID)
}

func waitContainerHealthy(ctx context.Context, networkID, endpointID string) bool {
	log := log.WithField("network.id", networkID).WithField("endpoint.id", endpointID)

	// Set up backoff configuration and initial delay.
	backoffConfig := newBackoff(1*time.Second, 1.5, 0, 10*time.Second)
	var delay time.Duration // zero: the first attempt is immediate.

	for {
		select {
		case <-ctx.Done():
			log.Warnf("waitContainerHealthy: operation is canceled: %v", context.Cause(ctx))
			return false

		case <-time.After(delay):
			containerID, err := getContainerByEndpointID(ctx, networkID, endpointID)
			if err != nil {
				delay = backoffConfig.NextBackOff()
				log.Warnf("waitContainerHealthy: failed to find the container for endpoint: %v. Retrying in %s", err, delay)
				continue
			}
			containerLog := log.WithField("container.id", containerID)

			container, err := dockerClient.ContainerInspect(ctx, containerID)
			if err != nil {
				delay = backoffConfig.NextBackOff()
				containerLog.Warnf("waitContainerHealthy: failed to get container details from Docker: %v. Retrying in %s", err, delay)
				continue
			}
			containerLog = containerLog.WithField("container.name", container.Name)

			if container.State == nil {
				delay = backoffConfig.NextBackOff()
				containerLog.Warnf("waitContainerHealthy: container state is not yet settled. Retrying in %s", delay)
				continue
			}

			stateHealth := "none"
			// If a healthcheck is defined for a container, wait for it to converge.
			// A `Running` but not `healthy` container must NOT be treated as ready.
			// Only containers without a healthcheck settle on the `Running` state.
			if container.State.Health != nil {
				if container.State.Health.Status == "healthy" {
					containerLog.Info("waitContainerHealthy: container is healthy")
					return true
				}
				stateHealth = container.State.Health.Status
			} else {
				if container.State.Running {
					containerLog.Info("waitContainerHealthy: container is running")
					return true
				}
			}

			delay = backoffConfig.NextBackOff()
			containerLog.Warnf("waitContainerHealthy: container is not ready (health=%s, status=%s). Retrying in %s", stateHealth, container.State.Status, delay)
		}
	}
}

func delContainerRoutes(containerID string) {
	networkFilter := filters.NewArgs()
	networkFilter.Add("driver", dockerDriverName)
	options := types.NetworkListOptions{
		Filters: networkFilter,
	}
	dockerNetworks, err := dockerClient.NetworkList(context.Background(), options)
	if err != nil {
		log.Errorf("delContainerRoutes: failed to get networks from Docker: %v", err)
		return
	}

	containerInspect, err := dockerClient.ContainerInspect(context.Background(), containerID)
	if err != nil {
		log.WithField("container.id", containerID).Errorf("delContainerRoutes: failed to get container details from Docker: %v", err)
		return
	}
	containerNetworks := containerInspect.NetworkSettings.Networks
	for _, containerNetwork := range containerNetworks {
		for _, network := range dockerNetworks {
			if network.ID == containerNetwork.NetworkID {
				go delRoute(containerNetwork.NetworkID, containerNetwork.EndpointID)
			}
		}
	}
}

func watchDockerEvents(ctx context.Context) {
	if os.Getenv("SIGUSR2_HANDLER") == "true" {
		log.Info("Enabling SIGUSR2 signal handler")

		SIGUSR2Enabled = true
		SIGUSR2Action = (os.Getenv("SIGUSR2_ACTION"))
	}

	// Set up backoff configuration and initial delay.
	backoffConfig := newBackoff(1*time.Second, 1.5, 0, 10*time.Second)
	var delay time.Duration // zero: the first attempt is immediate.

	// messages/errors == `nil` means we are (re)connecting.
	// [client.Client.Events] returns channels immediately; connection problems arrive on the error channel,
	// and both channels are closed by the client when the stream ends.
	// Setting them to `nil` on error/closure routes the next loop iteration
	// through the reconnect path with backoff.
	var messages <-chan events.Message
	var errors <-chan error

	for {
		if messages == nil || errors == nil {
			select {
			case <-ctx.Done():
				log.Warnf("watchDockerEvents: operation is canceled: %v", context.Cause(ctx))
				return

			case <-time.After(delay):
				eventFilters := filters.NewArgs(
					filters.Arg("type", "network"),
					filters.Arg("action", "create"),
					filters.Arg("action", "destroy"),
				)

				if SIGUSR2Enabled {
					eventFilters.Add("type", "container")
					eventFilters.Add("action", "kill")
				}

				eventOptions := types.EventsOptions{Filters: eventFilters}
				messages, errors = dockerClient.Events(ctx, eventOptions)
			}
			continue
		}

		select {
		case <-ctx.Done():
			log.Warnf("watchDockerEvents: operation is canceled: %v", context.Cause(ctx))
			return

		case err, ok := <-errors:
			delay = backoffConfig.NextBackOff()
			messages, errors = nil, nil

			if !ok {
				log.Warnf("watchDockerEvents: error stream has been closed. Restarting in %s", delay)
			} else {
				log.Warnf("watchDockerEvents: error has been received: %v. Restarting in %s", err, delay)
			}

			continue

		case event, ok := <-messages:
			if !ok {
				delay = backoffConfig.NextBackOff()
				log.Warnf("watchDockerEvents: event stream has been closed. Restarting in %s", delay)
				messages, errors = nil, nil
				continue
			}

			if event.Type == events.NetworkEventType {
				switch event.Action {
				case events.ActionCreate:
					handleDockerNetworkCreate(ctx, &event)
				case events.ActionDestroy:
					handleDockerNetworkDestroy(ctx, &event)
				}
			}
			if event.Type == events.ContainerEventType {
				if event.Action == events.ActionKill {
					if SIGUSR2Enabled {
						handleDockerContainerKill(&event)
					}
				}
			}

			// A successfully processed event proves the stream is healthy,
			// so reset the backoff for the next reconnect attempt.
			backoffConfig.Reset()
		}
	}
}

func handleDockerNetworkCreate(ctx context.Context, event *events.Message) {
	log := log.WithField("network.id", event.Actor.ID)
	network, err := dockerClient.NetworkInspect(ctx, event.Actor.ID, types.NetworkInspectOptions{})
	if err != nil {
		log.Errorf("handleDockerNetworkCreate: failed to get network details from Docker: %v", err)
		return
	}
	log = log.WithField("network.name", network.Name)
	if l, ok := network.Labels["bgplb_advertise"]; ok && l == "true" {
		for _, ipam := range network.IPAM.Config {
			if err := addAdvertisedSubnet(ctx, network.ID, ipam.Subnet); err == nil {
				log.WithField("subnet", ipam.Subnet).Info("handleDockerNetworkCreate: advertising the subnet")
			} else {
				log.WithField("subnet", ipam.Subnet).Errorf("handleDockerNetworkCreate: failed to advertise the subnet: %v", err)
			}
		}
	}
}

func handleDockerNetworkDestroy(ctx context.Context, event *events.Message) {
	log := log.WithField("network.id", event.Actor.ID)
	if err := delAdvertisedNetwork(ctx, event.Actor.ID); err == nil {
		log.Info("handleDockerNetworkDestroy: removed the advertised network")
	} else {
		log.Errorf("handleDockerNetworkDestroy: failed to remove the advertised network: %v", err)
	}
}

func handleDockerContainerKill(event *events.Message) {
	log := log.WithField("container.id", event.Actor.ID)
	if event.Actor.Attributes["signal"] == SIGUSR2Number {
		log.Info("SIGUSR2 signal received. Gracefully drain the load")
		delContainerRoutes(event.Actor.ID)

		if SIGUSR2Action == "stop" {
			log.Info("Stopping the container due to the SIGUSR2_ACTION=stop")
			go func() {
				time.Sleep(5 * time.Second)
				// Use the [context.Background] on purpose: the stop must proceed even if the
				// plugin's main context is being canceled (shutdown) at the same time.
				if err := dockerClient.ContainerStop(context.Background(), event.Actor.ID, container.StopOptions{Signal: "SIGTERM"}); err != nil {
					log.Errorf("handleDockerContainerKill: failed to stop the container: %v", err)
				}
			}()
		}
	}
}
