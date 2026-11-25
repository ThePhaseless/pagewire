package docker

import (
	"context"
	"log/slog"
	"net"
	"os"

	"github.com/moby/moby/client"
)

// Engine is the part of the Docker Engine API the backend uses. *client.Client
// implements it; tests use an in-memory fake.
type Engine interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerWait(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult
	CopyToContainer(ctx context.Context, containerID string, options client.CopyToContainerOptions) (client.CopyToContainerResult, error)
	ImageInspect(ctx context.Context, imageID string, inspectOpts ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePull(ctx context.Context, refStr string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	VolumeInspect(ctx context.Context, volumeID string, options client.VolumeInspectOptions) (client.VolumeInspectResult, error)
	VolumeCreate(ctx context.Context, options client.VolumeCreateOptions) (client.VolumeCreateResult, error)
	VolumeRemove(ctx context.Context, volumeID string, options client.VolumeRemoveOptions) (client.VolumeRemoveResult, error)
}

var _ Engine = (*client.Client)(nil)

// NewClient returns an Engine API client for host (unix:// or tcp://, for
// example a socket proxy), or for DOCKER_HOST and the SDK default when host is
// empty. The API version is negotiated on the first call; it makes no call
// itself. It warns when the Engine API would cross the network in clear text.
func NewClient(host string) (*client.Client, error) {
	opts := []client.Opt{client.FromEnv}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	c, err := client.New(opts...)
	if err != nil {
		return nil, err
	}
	// FromEnv turns TLS on exactly when DOCKER_CERT_PATH is set.
	if plainRemote(c.DaemonHost(), os.Getenv(client.EnvOverrideCertPath) != "") {
		slog.Warn("the Docker Engine API is reached over plain TCP: anyone on the network path gets root on that host; "+
			"use a unix socket, a loopback address or TLS (DOCKER_CERT_PATH)", "host", c.DaemonHost())
	}
	return c, nil
}

// plainRemote reports whether host is tcp:// to an address other than
// loopback, with no TLS.
func plainRemote(host string, tls bool) bool {
	u, err := client.ParseHostURL(host)
	if err != nil || u.Scheme != "tcp" || tls {
		return false
	}
	h, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		h = u.Host
	}
	if h == "localhost" {
		return false
	}
	ip := net.ParseIP(h)
	return ip == nil || !ip.IsLoopback()
}
