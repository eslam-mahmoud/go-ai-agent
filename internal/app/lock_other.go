//go:build !unix

package app

import "fmt"

func acquireInstanceLock(path string) (*InstanceLock, error) {
	return nil, fmt.Errorf("%w: %s", ErrLockUnsupported, path)
}

func releaseInstanceLock(_ *InstanceLock) error {
	return nil
}
