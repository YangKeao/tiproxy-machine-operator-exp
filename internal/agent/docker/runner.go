package docker

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

type Runner struct {
	Host string

	once    sync.Once
	cli     *client.Client
	initErr error
}

func NewRunner(host string) *Runner {
	return &Runner{Host: host}
}

type StartOptions struct {
	Name       string
	Image      string
	ConfigPath string
	CertDir    string
	ExtraArgs  []string
}

func (r *Runner) IsRunning(ctx context.Context, name string) (bool, error) {
	if name == "" {
		return false, fmt.Errorf("container name is required")
	}
	cli, err := r.getClient()
	if err != nil {
		return false, err
	}
	inspect, err := cli.ContainerInspect(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if inspect.State == nil {
		return false, nil
	}
	return inspect.State.Running && !inspect.State.Restarting, nil
}

func (r *Runner) Start(ctx context.Context, opts StartOptions) error {
	if opts.Name == "" {
		return fmt.Errorf("container name is required")
	}
	if opts.Image == "" {
		return fmt.Errorf("image is required")
	}
	if opts.ConfigPath == "" {
		return fmt.Errorf("config path is required")
	}

	cli, err := r.getClient()
	if err != nil {
		return err
	}

	pullResp, err := cli.ImagePull(ctx, opts.Image, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("docker pull %s failed: %w", opts.Image, err)
	}
	defer pullResp.Close()
	if _, err := io.Copy(io.Discard, pullResp); err != nil {
		return fmt.Errorf("drain image pull output: %w", err)
	}

	// Remove stale container if present.
	if err := cli.ContainerRemove(ctx, opts.Name, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("docker rm %s failed: %w", opts.Name, err)
	}

	binds := []string{fmt.Sprintf("%s:/etc/tiproxy/tiproxy.toml:ro", opts.ConfigPath)}
	if opts.CertDir != "" {
		binds = append(binds, fmt.Sprintf("%s:/etc/tiproxy/certs:ro", opts.CertDir))
	}

	cmd := []string{"--config", "/etc/tiproxy/tiproxy.toml"}
	cmd = append(cmd, opts.ExtraArgs...)

	createResp, err := cli.ContainerCreate(ctx, &container.Config{
		Image:      opts.Image,
		Entrypoint: []string{"/bin/tiproxy"},
		Cmd:        cmd,
	}, &container.HostConfig{
		Binds:       binds,
		NetworkMode: "host",
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
	}, nil, nil, opts.Name)
	if err != nil {
		return fmt.Errorf("docker create failed: %w", err)
	}

	if err := cli.ContainerStart(ctx, createResp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("docker start failed: %w", err)
	}
	return nil
}

func (r *Runner) getClient() (*client.Client, error) {
	r.once.Do(func() {
		opts := []client.Opt{
			client.FromEnv,
			client.WithAPIVersionNegotiation(),
		}
		if r.Host != "" {
			opts = append(opts, client.WithHost(r.Host))
		}
		r.cli, r.initErr = client.NewClientWithOpts(opts...)
	})
	if r.initErr != nil {
		return nil, r.initErr
	}
	return r.cli, nil
}
