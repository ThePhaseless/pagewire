package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"time"
)

// healthcheck GETs http://127.0.0.1:<port>/healthz, the port taken from
// --listen (env PAGEWIRE_LISTEN, as for serve). Any non-200 is an error.
func healthcheck(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	listen := fl.String("listen", env("PAGEWIRE_LISTEN", ":8080"), "serve's public listen address (env PAGEWIRE_LISTEN)")
	if err := fl.Parse(args); err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", *listen, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", port)+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}
