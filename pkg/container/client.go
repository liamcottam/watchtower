package container

import (
	"bytes"
	"fmt"
	"github.com/containrrr/watchtower/pkg/registry"
	"io/ioutil"
	"strings"
	"time"

	t "github.com/containrrr/watchtower/pkg/types"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	sdkClient "github.com/docker/docker/client"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

const defaultStopSignal = "SIGTERM"

// A Client is the interface through which watchtower interacts with the
// Docker API.
type Client interface {
	ListContainers(t.Filter) ([]Container, error)
	GetContainer(containerID string) (Container, error)
	StopContainer(Container, time.Duration) error
	StartContainer(Container) (string, error)
	RenameContainer(Container, string) error
	IsContainerStale(Container) (bool, error)
	ExecuteCommand(containerID string, command string) error
	RemoveImageByID(string) error

}

// NewClient returns a new Client instance which can be used to interact with
// the Docker API.
// The client reads its configuration from the following environment variables:
//  * DOCKER_HOST			the docker-engine host to send api requests to
//  * DOCKER_TLS_VERIFY		whether to verify tls certificates
//  * DOCKER_API_VERSION	the minimum docker api version to work with
func NewClient(pullImages bool, includeStopped bool, reviveStopped bool, removeVolumes bool) Client {
	cli, err := sdkClient.NewClientWithOpts(sdkClient.FromEnv)

	if err != nil {
		log.Fatalf("Error instantiating Docker client: %s", err)
	}

	return dockerClient{
		api:            cli,
		pullImages:     pullImages,
		removeVolumes:  removeVolumes,
		includeStopped: includeStopped,
		reviveStopped:  reviveStopped,
	}
}

type dockerClient struct {
	api            sdkClient.CommonAPIClient
	pullImages     bool
	removeVolumes  bool
	includeStopped bool
	reviveStopped  bool
}

func (client dockerClient) ListContainers(fn t.Filter) ([]Container, error) {
	cs := []Container{}
	bg := context.Background()

	if client.includeStopped {
		log.Debug("Retrieving containers including stopped and exited")
	} else {
		log.Debug("Retrieving running containers")
	}

	filter := client.createListFilter()
	containers, err := client.api.ContainerList(
		bg,
		types.ContainerListOptions{
			Filters: filter,
		})

	if err != nil {
		return nil, err
	}

	images, err := client.api.ImageList(bg, types.ImageListOptions{})
	if err != nil {
		return nil, err
	}

	for _, runningContainer := range containers {

		c, err := client.GetContainer(runningContainer.ID)
		if err != nil {
			return nil, err
		}

		if strings.HasPrefix(runningContainer.Image, "sha256") {
			for _, image := range images {
				if image.ID == runningContainer.ImageID && len(image.RepoTags) >= 1 {
					c.containerInfo.Config.Image = image.RepoTags[0]
				}
			}
		}

		if fn(c) {
			cs = append(cs, c)
		}
	}

	return cs, nil
}

func (client dockerClient) createListFilter() filters.Args {
	filterArgs := filters.NewArgs()
	filterArgs.Add("status", "running")

	if client.includeStopped {
		filterArgs.Add("status", "created")
		filterArgs.Add("status", "exited")
	}

	return filterArgs
}

func (client dockerClient) GetContainer(containerID string) (Container, error) {
	bg := context.Background()

	containerInfo, err := client.api.ContainerInspect(bg, containerID)
	if err != nil {
		return Container{}, err
	}

	imageInfo, _, err := client.api.ImageInspectWithRaw(bg, containerInfo.Image)
	if err != nil {
		return Container{}, err
	}

	container := Container{containerInfo: &containerInfo, imageInfo: &imageInfo}
	return container, nil
}

func (client dockerClient) StopContainer(c Container, timeout time.Duration) error {
	bg := context.Background()
	signal := c.StopSignal()
	if signal == "" {
		signal = defaultStopSignal
	}

	if c.IsRunning() {
		log.Infof("Stopping %s (%s) with %s", c.Name(), c.ID(), signal)
		if err := client.api.ContainerKill(bg, c.ID(), signal); err != nil {
			return err
		}
	}

	// TODO: This should probably be checked.
	_ = client.waitForStopOrTimeout(c, timeout)

	if c.containerInfo.HostConfig.AutoRemove {
		log.Debugf("AutoRemove container %s, skipping ContainerRemove call.", c.ID())
	} else {
		log.Debugf("Removing container %s", c.ID())

		if err := client.api.ContainerRemove(bg, c.ID(), types.ContainerRemoveOptions{Force: true, RemoveVolumes: client.removeVolumes}); err != nil {
			return err
		}
	}

	// Wait for container to be removed. In this case an error is a good thing
	if err := client.waitForStopOrTimeout(c, timeout); err == nil {
		return fmt.Errorf("container %s (%s) could not be removed", c.Name(), c.ID())
	}

	return nil
}

func (client dockerClient) StartContainer(c Container) (string, error) {
	bg := context.Background()
	config := c.runtimeConfig()
	hostConfig := c.hostConfig()
	networkConfig := &network.NetworkingConfig{EndpointsConfig: c.containerInfo.NetworkSettings.Networks}
	// simpleNetworkConfig is a networkConfig with only 1 network.
	// see: https://github.com/docker/docker/issues/29265
	simpleNetworkConfig := func() *network.NetworkingConfig {
		oneEndpoint := make(map[string]*network.EndpointSettings)
		for k, v := range networkConfig.EndpointsConfig {
			oneEndpoint[k] = v
			// we only need 1
			break
		}
		return &network.NetworkingConfig{EndpointsConfig: oneEndpoint}
	}()

	name := c.Name()

	log.Infof("Creating %s", name)
	createdContainer, err := client.api.ContainerCreate(bg, config, hostConfig, simpleNetworkConfig, name)
	if err != nil {
		return "", err
	}

	if !(hostConfig.NetworkMode.IsHost()) {

		for k := range simpleNetworkConfig.EndpointsConfig {
			err = client.api.NetworkDisconnect(bg, k, createdContainer.ID, true)
			if err != nil {
				return "", err
			}
		}

		for k, v := range networkConfig.EndpointsConfig {
			err = client.api.NetworkConnect(bg, k, createdContainer.ID, v)
			if err != nil {
				return "", err
			}
		}

	}

	if !c.IsRunning() && !client.reviveStopped {
		return createdContainer.ID, nil
	}

	return createdContainer.ID, client.doStartContainer(bg, c, createdContainer)

}

func (client dockerClient) doStartContainer(bg context.Context, c Container, creation container.ContainerCreateCreatedBody) error {
	name := c.Name()

	log.Debugf("Starting container %s (%s)", name, creation.ID)
	err := client.api.ContainerStart(bg, creation.ID, types.ContainerStartOptions{})
	if err != nil {
		return err
	}
	return nil
}

func (client dockerClient) RenameContainer(c Container, newName string) error {
	bg := context.Background()
	log.Debugf("Renaming container %s (%s) to %s", c.Name(), c.ID(), newName)
	return client.api.ContainerRename(bg, c.ID(), newName)
}

func (client dockerClient) IsContainerStale(container Container) (bool, error) {
	ctx := context.Background()

	if !client.pullImages {
		log.Debugf("Skipping image pull.")
	} else if err := client.PullImage(ctx, container); err != nil {
		return false, err
	}

	return client.HasNewImage(ctx, container)
}

func (client dockerClient) HasNewImage(ctx context.Context, container Container) (bool, error) {
	oldImageID := container.imageInfo.ID
	imageName := container.ImageName()

	newImageInfo, _, err := client.api.ImageInspectWithRaw(ctx, imageName)
	if err != nil {
		return false, err
	}

	if newImageInfo.ID == oldImageID {
		log.Debugf("No new images found for %s", container.Name())
		return false, nil
	}

	log.Infof("Found new %s image (%s)", imageName, newImageInfo.ID)
	return true, nil
}

func (client dockerClient) PullImage(ctx context.Context, container Container) error {
	containerName := container.Name()
	imageName := container.ImageName()
	log.Debugf("Pulling %s for %s", imageName, containerName)

	opts, err := registry.GetPullOptions(imageName)
	if err != nil {
		log.Debugf("Error loading authentication credentials %s", err)
		return err
	}

	response, err := client.api.ImagePull(ctx, imageName, opts)
	if err != nil {
		log.Debugf("Error pulling image %s, %s", imageName, err)
		return err
	}

	defer response.Close()
	// the pull request will be aborted prematurely unless the response is read
	if _, err = ioutil.ReadAll(response); err != nil {
		log.Error(err)
		return err
	}
	return nil
}

func (client dockerClient) RemoveImageByID(id string) error {
	log.Infof("Removing image %s", id)

	_, err := client.api.ImageRemove(
		context.Background(),
		id,
		types.ImageRemoveOptions{
			Force: true,
		})

	return err
}

func (client dockerClient) ExecuteCommand(containerID string, command string) error {
	bg := context.Background()

	// Create the exec
	execConfig := types.ExecConfig{
		Tty:    true,
		Detach: false,
		Cmd:    []string{"sh", "-c", command},
	}

	exec, err := client.api.ContainerExecCreate(bg, containerID, execConfig)
	if err != nil {
		return err
	}

	response, attachErr := client.api.ContainerExecAttach(bg, exec.ID, types.ExecStartCheck{
		Tty:    true,
		Detach: false,
	})
	if attachErr != nil {
		log.Errorf("Failed to extract command exec logs: %v", attachErr)
	}

	// Run the exec
	execStartCheck := types.ExecStartCheck{Detach: false, Tty: true}
	err = client.api.ContainerExecStart(bg, exec.ID, execStartCheck)
	if err != nil {
		return err
	}

	var execOutput string
	if attachErr == nil {
		defer response.Close()
		var writer bytes.Buffer
		written, err := writer.ReadFrom(response.Reader)
		if err != nil {
			log.Error(err)
		} else if written > 0 {
			execOutput = strings.TrimSpace(writer.String())
		}
	}

	// Inspect the exec to get the exit code and print a message if the
	// exit code is not success.
	execInspect, err := client.api.ContainerExecInspect(bg, exec.ID)
	if err != nil {
		return err
	}

	if execInspect.ExitCode > 0 {
		log.Errorf("Command exited with code %v.", execInspect.ExitCode)
		log.Error(execOutput)
	} else {
		if len(execOutput) > 0 {
			log.Infof("Command output:\n%v", execOutput)
		}
	}

	return nil
}

func (client dockerClient) waitForStopOrTimeout(c Container, waitTime time.Duration) error {
	bg := context.Background()
	timeout := time.After(waitTime)

	for {
		select {
		case <-timeout:
			return nil
		default:
			if ci, err := client.api.ContainerInspect(bg, c.ID()); err != nil {
				return err
			} else if !ci.State.Running {
				return nil
			}
		}

		time.Sleep(1 * time.Second)
	}
}
