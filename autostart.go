package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	windowsAutostartTask = "CodexQuotaRouter"
	linuxAutostartUnit   = "codex-quota-router.service"
	macOSAutostartLabel  = "com.codex-quota-router"
)

var errAutostartUnavailable = errors.New("autostart is unavailable")

func autostartEnabled() (bool, error) {
	switch runtime.GOOS {
	case "windows":
		output, err := exec.Command("schtasks.exe", "/Query", "/TN", windowsAutostartTask, "/XML").Output()
		if err != nil {
			return false, fmt.Errorf("%w: %v", errAutostartUnavailable, err)
		}
		return parseWindowsTaskEnabled(output)
	case "linux":
		output, err := exec.Command("systemctl", "--user", "is-enabled", linuxAutostartUnit).CombinedOutput()
		return parseLinuxAutostartState(output, err)
	case "darwin":
		if err := requireMacOSLaunchAgent(); err != nil {
			return false, err
		}
		target, err := macOSLaunchTarget()
		if err != nil {
			return false, err
		}
		output, err := exec.Command("launchctl", "print-disabled", target).Output()
		if err != nil {
			return false, fmt.Errorf("%w: %v", errAutostartUnavailable, err)
		}
		return !macOSAutostartDisabled(output), nil
	default:
		return false, errAutostartUnavailable
	}
}

func setAutostartEnabled(enabled bool) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		state := "/DISABLE"
		if enabled {
			state = "/ENABLE"
		}
		command = exec.Command("schtasks.exe", "/Change", "/TN", windowsAutostartTask, state)
	case "linux":
		state := "disable"
		if enabled {
			state = "enable"
		}
		command = exec.Command("systemctl", "--user", state, linuxAutostartUnit)
	case "darwin":
		if err := requireMacOSLaunchAgent(); err != nil {
			return err
		}
		target, err := macOSLaunchTarget()
		if err != nil {
			return err
		}
		state := "disable"
		if enabled {
			state = "enable"
		}
		command = exec.Command("launchctl", state, target+"/"+macOSAutostartLabel)
	default:
		return errAutostartUnavailable
	}
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %v: %s", errAutostartUnavailable, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func parseWindowsTaskEnabled(data []byte) (bool, error) {
	var task struct {
		Settings struct {
			Enabled string `xml:"Enabled"`
		} `xml:"Settings"`
	}
	if err := xml.Unmarshal(data, &task); err != nil {
		return false, fmt.Errorf("%w: %v", errAutostartUnavailable, err)
	}
	switch strings.ToLower(strings.TrimSpace(task.Settings.Enabled)) {
	case "", "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errAutostartUnavailable
	}
}

func parseLinuxAutostartState(output []byte, commandErr error) (bool, error) {
	switch strings.TrimSpace(string(output)) {
	case "enabled", "enabled-runtime", "linked", "linked-runtime", "alias":
		return true, nil
	case "disabled":
		return false, nil
	default:
		if commandErr != nil {
			return false, fmt.Errorf("%w: %v", errAutostartUnavailable, commandErr)
		}
		return false, errAutostartUnavailable
	}
}

func requireMacOSLaunchAgent() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("%w: %v", errAutostartUnavailable, err)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", macOSAutostartLabel+".plist")); err != nil {
		return fmt.Errorf("%w: %v", errAutostartUnavailable, err)
	}
	return nil
}

func macOSLaunchTarget() (string, error) {
	current, err := user.Current()
	if err != nil || current.Uid == "" {
		return "", fmt.Errorf("%w: current user", errAutostartUnavailable)
	}
	return "gui/" + current.Uid, nil
}

func macOSAutostartDisabled(output []byte) bool {
	return strings.Contains(string(output), `"`+macOSAutostartLabel+`" => true`)
}
