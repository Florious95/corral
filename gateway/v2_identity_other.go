//go:build !darwin

package main

import "fmt"

func v2ProcessStart(pid int) (v2ProcessStarted, error) {
	identity, err := v2ProcessIdentityForPID(pid)
	return identity.Started, err
}

func v2ProcessIdentityForPID(pid int) (v2ProcessIdentity, error) {
	return v2ProcessIdentity{}, fmt.Errorf("microsecond process identity is unavailable for pid %d on this platform", pid)
}
