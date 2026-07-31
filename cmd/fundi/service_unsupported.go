//go:build !darwin && !linux

package main

import (
	"errors"
	"git.graveland.dev/brent/fundi/internal/paths"
	"os"
	"path/filepath"
)

var errUnsupportedOS = errors.New("service management is not supported on this OS (only darwin and linux are implemented)")

type unsupportedBackend struct{}

func newServiceBackend() serviceBackend { return &unsupportedBackend{} }

func (b *unsupportedBackend) Install(_ serviceSpec) error { return errUnsupportedOS }
func (b *unsupportedBackend) Uninstall() error            { return errUnsupportedOS }
func (b *unsupportedBackend) Start() error                { return errUnsupportedOS }
func (b *unsupportedBackend) Stop() error                 { return errUnsupportedOS }
func (b *unsupportedBackend) Restart() error              { return errUnsupportedOS }
func (b *unsupportedBackend) Status() (serviceStatus, error) {
	return serviceStatus{}, errUnsupportedOS
}

func (b *unsupportedBackend) LogPath() string {
	home, _ := os.UserHomeDir()
	return paths.ServiceLogPath()
}
