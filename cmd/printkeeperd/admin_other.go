//go:build !darwin

package main

import (
	"context"
	"fmt"

	"github.com/phpfc/printkeeper/internal/queue"
)

// The daemon ships a darwin adapter only (see design doc §11.6: "Linux
// is CI-only in v1"). This stub keeps cmd/printkeeperd building and
// vetting on every GOOS the CI matrix covers without pretending to
// administer queues on a platform that has no adapter yet. Mirrors
// cmd/printkeeper's own admin_other.go.
type unsupportedAdmin struct{}

func newAdmin() queue.Admin {
	return unsupportedAdmin{}
}

func (unsupportedAdmin) List(context.Context) ([]queue.Queue, error) {
	return nil, fmt.Errorf("printer queue administration is not implemented on this platform yet")
}

func (unsupportedAdmin) Create(context.Context, string, string, string) error {
	return fmt.Errorf("printer queue administration is not implemented on this platform yet")
}

func (unsupportedAdmin) SetDeviceURI(context.Context, string, string) error {
	return fmt.Errorf("printer queue administration is not implemented on this platform yet")
}

func (unsupportedAdmin) Delete(context.Context, string) error {
	return fmt.Errorf("printer queue administration is not implemented on this platform yet")
}
