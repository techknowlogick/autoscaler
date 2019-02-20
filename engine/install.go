// Copyright 2018 Drone.IO Inc
// Use of this software is governed by the Business Source License
// that can be found in the LICENSE file.

package engine

import (
	"context"
	"fmt"
	"github.com/drone/autoscaler/config"
	"io"
	"io/ioutil"
	"sync"
	"time"
	"regexp"
	"strings"

	"github.com/drone/autoscaler"

	"docker.io/go-docker/api/types"
	"docker.io/go-docker/api/types/container"
	"github.com/rs/zerolog/log"
)

type installer struct {
	wg sync.WaitGroup

	image            string
	secret           string
	volumes          []string
	host             string
	proto            string
	keepaliveTime    time.Duration
	keepaliveTimeout time.Duration
	runner 	         config.Runner

	servers autoscaler.ServerStore
	client  clientFunc
}

func (i *installer) Install(ctx context.Context) error {
	logger := log.Ctx(ctx)

	servers, err := i.servers.ListState(ctx, autoscaler.StateCreated)
	if err != nil {
		return err
	}

	for _, server := range servers {
		server.State = autoscaler.StateStaging
		err = i.servers.Update(ctx, server)
		if err != nil {
			logger.Error().
				Err(err).
				Str("server", server.Name).
				Str("state", "staging").
				Msg("failed to update server state")
			return err
		}

		i.wg.Add(1)
		go func(server *autoscaler.Server) {
			i.install(ctx, server)
			i.wg.Done()
		}(server)
	}
	return nil
}

func (i *installer) install(ctx context.Context, instance *autoscaler.Server) error {
	logger := log.Ctx(ctx).With().
		Str("ip", instance.Address).
		Str("name", instance.Name).
		Logger()

	client, err := i.client(instance)
	if err != nil {
		logger.Error().Err(err).
			Msg("cannot create docker client")
		return i.errorUpdate(ctx, instance, err)
	}

	logger.Debug().
		Str("name", instance.Name).
		Msg("check docker connectivity")

	interval := time.Duration(0)
poller:
	for {
		select {
		case <-ctx.Done():
			logger.Debug().
				Str("name", instance.Name).
				Msg("connection timeout")

			return i.errorUpdate(ctx, instance, ctx.Err())
		case <-time.After(interval):
			interval = time.Minute

			logger.Debug().
				Str("name", instance.Name).
				Msg("connecting to docker")

			_, err := client.ContainerList(ctx, types.ContainerListOptions{})
			if err != nil {
				logger.Debug().
					Str("error", err.Error()).
					Str("name", instance.Name).
					Msgf("cannot connect, retry in %v", interval)
				continue
			}
			break poller
		}
	}

	logger.Debug().
		Str("image", i.image).
		Msg("pull docker image")

	rc, err := client.ImagePull(ctx, i.image, types.ImagePullOptions{})
	if err != nil {
		logger.Error().Err(err).
			Str("image", i.image).
			Msg("cannot pull docker image")
		return i.errorUpdate(ctx, instance, err)
	}
	io.Copy(ioutil.Discard, rc)
	rc.Close()

	logger.Debug().
		Str("image", i.image).
		Msg("create agent container")

	i.volumes = append(i.volumes, "/var/run/docker.sock:/var/run/docker.sock")
	res, err := client.ContainerCreate(ctx,
		&container.Config{
			Image:        i.image,
			AttachStdout: true,
			AttachStderr: true,
			Env: []string{
				fmt.Sprintf("DRONE_RPC_SERVER=%s://%s", i.proto, i.host),
				fmt.Sprintf("DRONE_RPC_SECRET=%s", i.secret),
				fmt.Sprintf("DRONE_RUNNER_CAPACITY=%v", instance.Capacity),
				fmt.Sprintf("DRONE_RUNNER_NAME=%s", instance.Name),
				fmt.Sprintf("DRONE_RUNNER_VOLUMES=%s", i.runner.Volumes),
				fmt.Sprintf("DRONE_RUNNER_DEVICES=%s", i.runner.Devices),
				fmt.Sprintf("DRONE_RUNNER_PRIVILEGED_IMAGES=%s", i.runner.Privileged),
			},
			Volumes: toVol(i.volumes),
			Labels: map[string]string{
				"com.centurylinklabs.watchtower.enable":      "true",
				"com.centurylinklabs.watchtower.stop-signal": "SIGHUP",
				"io.drone.agent.name":                        instance.Name,
				"io.drone.agent.zone":                        instance.Region,
				"io.drone.agent.size":                        instance.Size,
				"io.drone.agent.instance":                    instance.ID,
				"io.drone.agent.capacity":                    fmt.Sprint(instance.Capacity),
			},
		},
		&container.HostConfig{
			Binds: i.volumes,
			RestartPolicy: container.RestartPolicy{
				Name: "always",
			},
		}, nil, "agent")

	if err != nil {
		logger.Error().Err(err).
			Str("image", i.image).
			Msg("cannot create agent container")
		return i.errorUpdate(ctx, instance, err)
	}

	logger.Debug().
		Str("image", i.image).
		Msg("start the agent container")

	err = client.ContainerStart(ctx, res.ID, types.ContainerStartOptions{})
	if err != nil {
		logger.Debug().
			Str("image", i.image).
			Msg("cannot start the agent container")
		return i.errorUpdate(ctx, instance, err)
	}

	logger.Debug().
		Str("image", i.image).
		Msg("agent container started")

	instance.State = autoscaler.StateRunning
	return i.servers.Update(ctx, instance)
}

func (i *installer) errorUpdate(ctx context.Context, server *autoscaler.Server, err error) error {
	if err != nil {
		server.State = autoscaler.StateError
		server.Error = err.Error()
		i.servers.Update(ctx, server)
	}
	return err
}

// helper function that converts a slice of volume paths to a set of
// unique volume names.
func toVol(paths []string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, path := range paths {
		parts, err := splitVolumeParts(path)
		if err != nil {
			continue
		}
		if len(parts) < 2 {
			continue
		}
		set[parts[1]] = struct{}{}
	}
	return set
}

// helper function that split volume path
func splitVolumeParts(volumeParts string) ([]string, error) {
	pattern := `^((?:[\w]\:)?[^\:]*)\:((?:[\w]\:)?[^\:]*)(?:\:([rwom]*))?`
	r, err := regexp.Compile(pattern)
	if err != nil {
		return []string{}, err
	}
	if r.MatchString(volumeParts) {
		results := r.FindStringSubmatch(volumeParts)[1:]
		cleanResults := []string{}
		for _, item := range results {
			if item != "" {
				cleanResults = append(cleanResults, item)
			}
		}
		return cleanResults, nil
	} else {
		return strings.Split(volumeParts, ":"), nil
	}
}