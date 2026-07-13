//go:build darwin

package main

import "os"

func protectConfig(data []byte) ([]byte, error) {
	return append([]byte(nil), data...), nil
}

func unprotectConfig(data []byte) ([]byte, error) {
	return append([]byte(nil), data...), nil
}

func replaceFile(source, target string) error {
	return os.Rename(source, target)
}
