package main

// One way to reach the running daemon's admin API, shared by `spillway
// status` and anything else that needs it.
//
// The address comes from config and can be a unix socket, so "http://"+addr
// is not enough on its own: a socket path has to be dialled directly with the
// host in the URL reduced to a placeholder. The token file is read when it
// exists — a loopback listener needs none.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
)

type adminAPI struct {
	client *http.Client
	base   string
	token  string
}

// dialAdmin resolves the configured admin listener into a client for it.
func dialAdmin() (*adminAPI, error) {
	cfgPath, err := config.Path()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	addr := cfg.Admin.Addr
	if addr == "" {
		addr = admin.DefaultAddr
	}

	api := &adminAPI{client: &http.Client{Timeout: 5 * time.Second}, base: "http://" + addr}
	if admin.IsUnix(addr) {
		sock := admin.SocketPath(addr)
		api.base = "http://unix"
		api.client.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		}
	}
	if b, err := os.ReadFile(tokenPathFor(cfgPath)); err == nil {
		api.token = strings.TrimSpace(string(b))
	}
	return api, nil
}

// get decodes one admin endpoint into v.
func (a *adminAPI) get(path string, v any) error {
	req, err := http.NewRequest(http.MethodGet, a.base+path, nil)
	if err != nil {
		return err
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("admin API unreachable (is `spillway server` running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("admin API %s: %d", path, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}
