//go:build !darwin

package main

func nativeProcSnapshotAll() (*procSnapshot, error, bool) {
	return nil, nil, false
}

func nativeProcSnapshotTree(int) (*procSnapshot, error, bool) {
	return nil, nil, false
}

func nativeProcessEnvironmentValue(int, string) (string, bool) {
	return "", false
}
