package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/ioutil"
	"strings"

	"github.com/drone/drone-runtime/engine"
	"github.com/drone/drone-runtime/engine/docker/authutil"
	"github.com/drone/drone-runtime/engine/docker/stdcopy"

	"docker.io/go-docker"
	"docker.io/go-docker/api/types"
	"docker.io/go-docker/api/types/volume"
	"github.com/docker/distribution/reference"
)

type dockerEngine struct {
	spec   *engine.Spec
	client docker.APIClient
}

func (e *dockerEngine) Setup(ctx context.Context) error {
	if e.spec.Docker != nil {
		// creates the default temporary (local) volumes
		// that are mounted into each container step.
		for _, vol := range e.spec.Docker.Volumes {
			if vol.EmptyDir == nil {
				continue
			}

			_, err := e.client.VolumeCreate(ctx, volume.VolumesCreateBody{
				Name:   vol.Metadata.UID,
				Driver: "local",
				Labels: e.spec.Metadata.Labels,
			})
			if err != nil {
				return err
			}
		}
	}

	// creates the default pod network. All containers
	// defined in the pipeline are attached to this network.
	_, err := e.client.NetworkCreate(ctx, e.spec.Metadata.UID, types.NetworkCreate{
		Driver: "bridge",
		Labels: e.spec.Metadata.Labels,
	})

	return err
}

func (e *dockerEngine) Create(ctx context.Context, step *engine.Step) error {
	if step.Docker == nil {
		return errors.New("engine: missing docker configuration")
	}

	// parse the docker image name. We need to extract the
	// image domain name and match to registry credentials
	// stored in the .docker/config.json object.
	_, domain, latest, err := parseImage(step.Docker.Image)
	if err != nil {
		return err
	}

	// create pull options with encoded authorization credentials.
	pullopts := types.ImagePullOptions{}
	auth, ok := engine.LookupAuth(e.spec, domain)
	if ok {
		pullopts.RegistryAuth = authutil.Encode(auth.Username, auth.Password)
	}

	// automatically pull the latest version of the image if requested
	// by the process configuration.
	if step.Docker.PullPolicy == engine.PullAlways ||
		(step.Docker.PullPolicy == engine.PullDefault && latest) {
		// TODO(bradrydzewski) implement the PullDefault strategy to pull
		// the image if the tag is :latest
		rc, perr := e.client.ImagePull(ctx, step.Docker.Image, pullopts)
		if perr == nil {
			io.Copy(ioutil.Discard, rc)
			rc.Close()
		}
		if perr != nil {
			return perr
		}
	}

	_, err = e.client.ContainerCreate(ctx,
		toConfig(e.spec, step),
		toHostConfig(e.spec, step),
		toNetConfig(e.spec, step),
		step.Metadata.UID,
	)

	// automatically pull and try to re-create the image if the
	// failure is caused because the image does not exist.
	if docker.IsErrImageNotFound(err) && step.Docker.PullPolicy != engine.PullNever {
		rc, perr := e.client.ImagePull(ctx, step.Docker.Image, pullopts)
		if perr != nil {
			return perr
		}
		io.Copy(ioutil.Discard, rc)
		rc.Close()

		// once the image is successfully pulled we attempt to
		// re-create the container.
		_, err = e.client.ContainerCreate(ctx,
			toConfig(e.spec, step),
			toHostConfig(e.spec, step),
			toNetConfig(e.spec, step),
			step.Metadata.UID,
		)
	}
	if err != nil {
		return err
	}

	copyOpts := types.CopyToContainerOptions{}
	copyOpts.AllowOverwriteDirWithFile = false
	for _, mount := range step.Files {
		file, ok := engine.LookupFile(e.spec, mount.Name)
		if !ok {
			continue
		}
		tar := createTarfile(file, mount)

		// TODO(bradrydzewski) this path is probably different on windows.
		err := e.client.CopyToContainer(ctx, step.Metadata.UID, "/", bytes.NewReader(tar), copyOpts)
		if err != nil {
			return err
		}
	}

	return nil
}

func (e *dockerEngine) Start(ctx context.Context, step *engine.Step) error {
	return e.client.ContainerStart(ctx, step.Metadata.UID, types.ContainerStartOptions{})
}

func (e *dockerEngine) Wait(ctx context.Context, step *engine.Step) (*engine.State, error) {
	wait, errc := e.client.ContainerWait(ctx, step.Metadata.UID, "")
	select {
	case <-wait:
	case <-errc:
	}

	info, err := e.client.ContainerInspect(ctx, step.Metadata.UID)
	if err != nil {
		return nil, err
	}
	if info.State.Running {
		// TODO(bradrydewski) if the state is still running
		// we should call wait again.
	}

	return &engine.State{
		Exited:    true,
		ExitCode:  info.State.ExitCode,
		OOMKilled: info.State.OOMKilled,
	}, nil
}

func (e *dockerEngine) Tail(ctx context.Context, step *engine.Step) (io.ReadCloser, error) {
	opts := types.ContainerLogsOptions{
		Follow:     true,
		ShowStdout: true,
		ShowStderr: true,
		Details:    false,
		Timestamps: false,
	}

	logs, err := e.client.ContainerLogs(ctx, step.Metadata.UID, opts)
	if err != nil {
		return nil, err
	}
	rc, wc := io.Pipe()

	go func() {
		stdcopy.StdCopy(wc, wc, logs)
		logs.Close()
		wc.Close()
		rc.Close()
	}()
	return rc, nil
}

func (e *dockerEngine) Destroy(ctx context.Context) error {
	removeOpts := types.ContainerRemoveOptions{
		Force:         true,
		RemoveLinks:   false,
		RemoveVolumes: true,
	}

	// cleanup all containers
	for _, step := range e.spec.Steps {
		e.client.ContainerKill(ctx, step.Metadata.UID, "9")
		e.client.ContainerRemove(ctx, step.Metadata.UID, removeOpts)
	}

	// cleanup all volumes
	if e.spec.Docker != nil {
		for _, vol := range e.spec.Docker.Volumes {
			if vol.EmptyDir == nil {
				continue
			}
			err := e.client.VolumeRemove(ctx, vol.Metadata.UID, true)
			if err != nil {
				return err
			}
		}
	}

	// cleanup the network
	return e.client.NetworkRemove(ctx, e.spec.Metadata.UID)
}

// helper function parses the image and returns the
// canonical image name, domain name, and whether or not
// the image tag is :latest.
func parseImage(s string) (canonical, domain string, latest bool, err error) {
	// parse the docker image name. We need to extract the
	// image domain name and match to registry credentials
	// stored in the .docker/config.json object.
	named, err := reference.ParseNormalizedNamed(s)
	if err != nil {
		return
	}
	// the canonical image name, for some reason, excludes
	// the tag name. So we need to make sure it is included
	// in the image name so we can determine if the :latest
	// tag is specified
	named = reference.TagNameOnly(named)

	return named.String(),
		reference.Domain(named),
		strings.HasSuffix(named.String(), ":latest"),
		nil
}
