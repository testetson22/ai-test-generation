package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/httpapi"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	handler, addr, err := serverConfiguration(os.Getenv("ATG_MODE"), os.Getenv("ATG_LISTEN_ADDR"))
	if err != nil {
		logger.Error("invalid control service configuration", "error", err)
		os.Exit(2)
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("control service listening", "address", addr)
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("control service stopped", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
			os.Exit(1)
		}
	}
}

func serverConfiguration(mode, configuredAddress string) (http.Handler, string, error) {
	if mode == "" {
		address := configuredAddress
		if address == "" {
			address = ":8080"
		}
		return httpapi.NewHandler(httpapi.Dependencies{}), address, nil
	}
	if mode != "offline-simulation" {
		return nil, "", errors.New("ATG_MODE must be empty or offline-simulation; production composition is not available")
	}
	address := configuredAddress
	if address == "" {
		address = "127.0.0.1:8080"
	}
	if !loopbackAddress(address) {
		return nil, "", errors.New("offline-simulation mode requires ATG_LISTEN_ADDR to use a loopback IP address")
	}
	mux := http.NewServeMux()
	mux.Handle("/", httpapi.NewHandler(httpapi.Dependencies{}))
	mux.Handle("POST /development/simulations/test-generation", httpapi.NewTestGenerationSimulationHandler())
	return mux, address, nil
}

func loopbackAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	portNumber, err := strconv.Atoi(port)
	return ip != nil && ip.IsLoopback() && err == nil && portNumber > 0 && portNumber <= 65535
}
