//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	cryptProtectUIForbidden = 0x1
	moveFileReplaceExisting = 0x1
	moveFileWriteThrough    = 0x8
)

type dataBlob struct {
	size uint32
	data *byte
}

var (
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	cryptProtectData   = crypt32.NewProc("CryptProtectData")
	cryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	localFree          = kernel32.NewProc("LocalFree")
	moveFileEx         = kernel32.NewProc("MoveFileExW")
)

func protectConfig(data []byte) ([]byte, error) {
	return cryptData(data, cryptProtectData)
}

func unprotectConfig(data []byte) ([]byte, error) {
	return cryptData(data, cryptUnprotectData)
}

func cryptData(data []byte, proc *syscall.LazyProc) ([]byte, error) {
	input := dataBlob{size: uint32(len(data))}
	if len(data) != 0 {
		input.data = &data[0]
	}
	var output dataBlob
	result, _, callErr := proc.Call(
		uintptr(unsafe.Pointer(&input)),
		0,
		0,
		0,
		0,
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&output)),
	)
	if result == 0 {
		if callErr != syscall.Errno(0) {
			return nil, callErr
		}
		return nil, fmt.Errorf("DPAPI operation failed")
	}
	defer localFree.Call(uintptr(unsafe.Pointer(output.data)))
	return append([]byte(nil), unsafe.Slice(output.data, int(output.size))...), nil
}

func replaceFile(source, target string) error {
	sourcePtr, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetPtr, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	result, _, callErr := moveFileEx.Call(
		uintptr(unsafe.Pointer(sourcePtr)),
		uintptr(unsafe.Pointer(targetPtr)),
		moveFileReplaceExisting|moveFileWriteThrough,
	)
	if result == 0 {
		if callErr != syscall.Errno(0) {
			return callErr
		}
		return fmt.Errorf("atomic config replacement failed")
	}
	return nil
}
