package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const trayShutdownTimeout = 5 * time.Second

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
