//go:build linux

package main

import "github.com/godbus/dbus/v5"

func trayAvailable() bool {
	connection, err := dbus.SessionBusPrivateNoAutoStartup()
	if err != nil {
		return false
	}
	defer connection.Close()
	if err := connection.Auth(nil); err != nil {
		return false
	}
	return connection.Hello() == nil
}
