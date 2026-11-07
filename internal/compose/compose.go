// Package compose drives the Docker-compatible compose client, which on this
// host is Podman's socket.
package compose

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Meillaya/celine/internal/execx"
)

// Client wraps the docker CLI against a compose file.
type Client struct {
	Runner  execx.Runner
	Docker  string
	File    string
	Project string
	Stdout  io.Writer
	Stderr  io.Writer
}

// New builds a client for one rendered compose file.
func New(runner execx.Runner, file string) *Client {
	return &Client{
		Runner:  runner,
		Docker:  "docker",
		File:    file,
		Project: "celine",
	}
}

func (c *Client) options() execx.Options {
	return execx.Options{Stdout: c.Stdout, Stderr: c.Stderr}
}

func (c *Client) run(ctx context.Context, args ...string) (execx.Result, error) {
	full := append([]string{"compose", "--project-name", c.Project, "-f", c.File}, args...)
	return c.Runner.Run(ctx, c.Docker, full, c.options())
}

// Up starts the stack.
func (c *Client) Up(ctx context.Context) error {
	if _, err := c.run(ctx, "up", "-d", "--remove-orphans"); err != nil {
		return err
	}
	return nil
}

// Down stops and removes the containers while keeping bind-mounted state.
func (c *Client) Down(ctx context.Context) error {
	_, err := c.run(ctx, "down", "--remove-orphans")
	return err
}

// Build builds the named services.
func (c *Client) Build(ctx context.Context, services ...string) error {
	args := append([]string{"build"}, services...)
	_, err := c.run(ctx, args...)
	return err
}

// Pull pulls the named services, or all when none are given.
func (c *Client) Pull(ctx context.Context, services ...string) error {
	args := append([]string{"pull"}, services...)
	_, err := c.run(ctx, args...)
	return err
}

// ConfigQuiet validates the compose file without starting anything.
func (c *Client) ConfigQuiet(ctx context.Context) error {
	_, err := c.run(ctx, "config", "--quiet")
	return err
}

// Logs streams or prints container logs.
func (c *Client) Logs(ctx context.Context, follow bool, service string) error {
	args := []string{"logs", "--tail=200"}
	if follow {
		args = append(args, "-f")
	}
	if service != "" {
		args = append(args, service)
	}
	_, err := c.run(ctx, args...)
	return err
}

// ImageExists reports whether the runtime already has an image.
func (c *Client) ImageExists(ctx context.Context, image string) (bool, error) {
	result, err := c.Runner.Run(ctx, c.Docker, []string{"image", "inspect", image}, execx.Options{})
	if err != nil {
		if result.ExitCode != 0 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// RuntimeInfo returns a short description of the container runtime.
func (c *Client) RuntimeInfo(ctx context.Context) (string, error) {
	result, err := c.Runner.Run(ctx, c.Docker, []string{"info", "--format", "{{.ServerVersion}} {{.OperatingSystem}}"}, execx.Options{})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}

// ComposeVersion reports the compose client version.
func (c *Client) ComposeVersion(ctx context.Context) (string, error) {
	result, err := c.Runner.Run(ctx, c.Docker, []string{"compose", "version"}, execx.Options{})
	if err != nil {
		return "", fmt.Errorf("docker compose is not available: %w", err)
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}
