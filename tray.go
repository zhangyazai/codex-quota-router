package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"
)

const (
	managementURL       = "http://" + listenAddress + "/"
	trayShutdownTimeout = 5 * time.Second

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

func serve(server *http.Server, listener net.Listener) error {
	loop := runHeadless
	if trayAvailable() {
		loop = runSystemTray
	}
	return serveWithStopLoop(server, listener, loop)
}

func serveWithStopLoop(server *http.Server, listener net.Listener, loop func(func(), <-chan struct{}) error) error {
	baseContext, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	server.BaseContext = func(net.Listener) context.Context {
		return baseContext
	}

	serverDone := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = server.Serve(listener)
		close(serverDone)
	}()

	stopRequested := make(chan struct{})
	shutdownDone := make(chan struct{})
	var stopOnce sync.Once
	requestStop := func() {
		stopOnce.Do(func() {
			close(stopRequested)
			go func() {
				defer close(shutdownDone)
				cancelRequests()
				shutdownContext, cancel := context.WithTimeout(context.Background(), trayShutdownTimeout)
				defer cancel()
				if err := server.Shutdown(shutdownContext); err != nil {
					_ = server.Close()
				}
			}()
		})
	}
	signalContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	go func() {
		select {
		case <-signalContext.Done():
			requestStop()
		case <-serverDone:
		}
	}()

	loopErr := loop(requestStop, serverDone)
	loopFailed := false
	if loopErr != nil {
		select {
		case <-stopRequested:
		default:
			loopFailed = true
			requestStop()
		}
	}
	select {
	case <-serverDone:
	default:
		if loopErr == nil {
			loopErr = errSystemTrayStopped
			loopFailed = true
		}
		requestStop()
	}
	<-serverDone
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		if loopFailed {
			<-shutdownDone
		}
		return serveErr
	}
	if loopFailed {
		<-shutdownDone
		return loopErr
	}
	select {
	case <-stopRequested:
		<-shutdownDone
		return nil
	default:
		return serveErr
	}
}

func runHeadless(_ func(), serverDone <-chan struct{}) error {
	<-serverDone
	return nil
}

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
		systray.AddSeparator()
		quitItem := systray.AddMenuItem("退出", "停止 Codex Quota Router")
		go func() {
			for {
				select {
				case <-openItem.ClickedCh:
					openManagementPage()
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
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", managementURL)
	case "darwin":
		command = exec.Command("open", managementURL)
	default:
		if path, err := exec.LookPath("xdg-open"); err == nil {
			command = exec.Command(path, managementURL)
		} else if path, err := exec.LookPath("gio"); err == nil {
			command = exec.Command(path, "open", managementURL)
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
