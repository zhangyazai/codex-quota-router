package main

import (
	"encoding/base64"
	"errors"
	"io"
	"log"
	"os/exec"
	"runtime"

	"fyne.io/systray"
)

const (
	managementURL = "http://" + listenAddress + "/"
	projectURL     = "https://github.com/zhangyazai/codex-quota-router"

	trayRegularPNGBase64  = "iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAYAAABzenr0AAAAiklEQVR42u3XSw6AMAgE0F7Gk3n8HkLjzpjaQhk+VUjcOi+pIC0li1nbXo/e4xKqipkNFyOkwSIIOpyNQIZexQKgw++AIaL3EkmRj+LfgNE5IgFNhEbbuQOebegCIA+lBHDOzxSghSDNArMZkIA3hOmPqIVAf4TsjQjdAesspSHW8jAXkxBXs0/XCZdB7ZNjHgT5AAAAAElFTkSuQmCC"
	trayTemplatePNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAYAAABzenr0AAAAO0lEQVR42mNgGAWjYBSMAPB/oC3/Ty1DKMGjDhjaDhjwRDiks+EoGPrxN2iy0WgpOOqA0WJ0FIyC4QsA6GqTbbQssMIAAAAASUVORK5CYII="
	trayWindowsICOBase64  = "AAABAAEAICAAAAEAIADDAAAAFgAAAIlQTkcNChoKAAAADUlIRFIAAAAgAAAAIAgGAAAAc3p69AAAAIpJREFUeNrt10sOgDAIBNBexpN5/B5C486Y2kIZPlVI3DovqSAtJYtZ216P3uMSqoqZDRcjpMEiCDqcjUCGXsUCoMPvgCGi9xJJkY/i34DROSIBTYRG27kDnm3oAiAPpQRwzs8UoIUgzQKzGZCAN4Tpj6iFQH+E7I0I3QHrLKUh1vIwF5MQV7NP1wmXQe2TYx4E+QAAAABJRU5ErkJggg=="
)

var (
	errSystemTrayStopped = errors.New("system tray stopped unexpectedly")
	trayRegularPNG       = decodeTrayIcon(trayRegularPNGBase64)
	trayTemplatePNG      = decodeTrayIcon(trayTemplatePNGBase64)
	trayWindowsICO       = decodeTrayIcon(trayWindowsICOBase64)
)

func runSystemTray(requestStop func(), serverDone <-chan struct{}) (err error) {
	log.SetOutput(io.Discard)
	defer func() {
		if recover() != nil {
			err = errSystemTrayStopped
		}
	}()
	systray.Run(func() {
		regularIcon := trayRegularPNG
		if runtime.GOOS == "windows" {
			regularIcon = trayWindowsICO
		}
		systray.SetTemplateIcon(trayTemplatePNG, regularIcon)
		systray.SetTooltip("Codex Quota Router")
		openItem := systray.AddMenuItem("打开管理页", managementURL)
		autostartItem := systray.AddMenuItemCheckbox("开机自启动", "当前用户登录后自动启动", false)
		if enabled, stateErr := autostartEnabled(); stateErr != nil {
			autostartItem.Disable()
		} else if enabled {
			autostartItem.Check()
		}
		versionItem := systray.AddMenuItem("版本 "+applicationVersion, "Codex Quota Router 当前版本")
		versionItem.Disable()
		aboutItem := systray.AddMenuItem("关于（GitHub）", projectURL)
		systray.AddSeparator()
		quitItem := systray.AddMenuItem("退出", "停止 Codex Quota Router")
		go func() {
			for {
				select {
				case <-openItem.ClickedCh:
					openManagementPage()
				case <-autostartItem.ClickedCh:
					enabled := !autostartItem.Checked()
					if setAutostartEnabled(enabled) != nil {
						continue
					}
					if enabled {
						autostartItem.Check()
					} else {
						autostartItem.Uncheck()
					}
				case <-aboutItem.ClickedCh:
					openURL(projectURL)
				case <-quitItem.ClickedCh:
					requestStop()
					return
				case <-serverDone:
					return
				}
			}
		}()
		go func() {
			<-serverDone
			systray.Quit()
		}()
	}, nil)
	select {
	case <-serverDone:
		return nil
	default:
		return errSystemTrayStopped
	}
}

func openManagementPage() {
	openURL(managementURL)
}

func openURL(target string) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		command = exec.Command("open", target)
	default:
		if path, err := exec.LookPath("xdg-open"); err == nil {
			command = exec.Command(path, target)
		} else if path, err := exec.LookPath("gio"); err == nil {
			command = exec.Command(path, "open", target)
		}
	}
	if command == nil {
		return
	}
	if command.Start() != nil {
		return
	}
	go func() {
		_ = command.Wait()
	}()
}

func decodeTrayIcon(encoded string) []byte {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		panic(err)
	}
	return data
}
