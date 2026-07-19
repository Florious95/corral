//go:build darwin

package main

import "golang.org/x/sys/unix"

func v2ProcessStart(pid int) (v2ProcessStarted, error) {
	identity, err := v2ProcessIdentityForPID(pid)
	return identity.Started, err
}

func v2ProcessIdentityForPID(pid int) (v2ProcessIdentity, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return v2ProcessIdentity{}, err
	}
	return v2ProcessIdentity{
		Started:   v2ProcessStarted{Sec: info.Proc.P_starttime.Sec, Usec: int64(info.Proc.P_starttime.Usec)},
		ParentPID: int(info.Eproc.Ppid),
	}, nil
}
