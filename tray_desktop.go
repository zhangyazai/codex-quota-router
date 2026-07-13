//go:build windows || darwin

package main

func trayAvailable() bool {
	return true
}
