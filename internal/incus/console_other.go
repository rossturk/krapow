//go:build !linux

package incus

import (
	"errors"
	"os"
)

func openPTY() (*os.File, *os.File, error) {
	return nil, nil, errors.New("openPTY: only supported on linux")
}
