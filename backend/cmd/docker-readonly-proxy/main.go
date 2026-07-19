// docker-readonly-proxy is a tiny filtering proxy in front of the host
// Docker engine socket. It exposes a minimal HTTP API over the docker
// network so the admin portal can read container status without the admin
// container itself mounting /var/run/docker.sock (which would be
// root-equivalent on the host).
//
// Allowlist (everything else returns 403):
//
//	GET /containers/json
//	GET /info
//
// The socket is mounted read-only into this container; the proxy only
// needs to read. No host port is published — other containers reach it at
// http://docker-proxy:2375.
//
// Env vars:
//
//	DOCKER_PROXY_ADDR  listen address (default: :2375)
//	DOCKER_HOST        docker host URL (default: unix:///var/run/docker.sock)
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// allowlist maps exact "METHOD /path" to true. No glob, no prefix match —
// /containers/json?all=true passes because the query string is not part of
// the path; /containers/json/anything does not.
var allowlist = map[string]bool{
	"GET /containers/json": true,
	"GET /info":            true,
}

func main() {
	addr := envOr("DOCKER_PROXY_ADDR", ":2375")
	dockerHost := envOr("DOCKER_HOST", "unix:///var/run/docker.sock")

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	client, err := newDockerClient(dockerHost)
	if err != nil {
		logger.Error("invalid DOCKER_HOST", "err", err, "value", dockerHost)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if !allowlist[key] {
			logger.Warn("denied", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		proxy(w, r, client, logger)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		srv.Shutdown(shutdownCtx)
	}()

	logger.Info("docker-readonly-proxy starting", "addr", addr, "docker_host", dockerHost)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// proxy forwards the allowed request to the Docker engine and copies the
// response back. We do not forward arbitrary headers — Docker's API only
// needs the method + path + query. The response body is streamed.
func proxy(w http.ResponseWriter, r *http.Request, client *http.Client, logger *slog.Logger) {
	upstream := "http://docker" + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("upstream call", "err", err)
		http.Error(w, "docker engine unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// newDockerClient builds an *http.Client that dials the configured Docker
// host. Only unix:// is supported in v1 — tcp:// can be added later.
func newDockerClient(dockerHost string) (*http.Client, error) {
	u, err := url.Parse(dockerHost)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "unix":
		socket := u.Path
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		}
		return &http.Client{Transport: tr, Timeout: 10 * time.Second}, nil
	case "tcp", "http":
		return nil, fmt.Errorf("tcp/docker host not supported: %s", dockerHost)
	default:
		return nil, fmt.Errorf("unsupported DOCKER_HOST scheme %q", u.Scheme)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}