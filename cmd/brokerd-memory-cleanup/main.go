// brokerd-memory-cleanup inventories smoke residue; deletion requires -execute and a reviewed manifest.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/danieliser/capability-broker/pkg/memorycleanup"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}
func run() error {
	endpoint := flag.String("url", "http://127.0.0.1:8890/mcp/memory", "broker memory endpoint")
	manifest := flag.String("manifest", "", "reviewed inventory JSON (omit to enumerate)")
	execute := flag.Bool("execute", false, "delete exactly the reviewed manifest IDs; default reads only")
	flag.Parse()
	if *execute && *manifest == "" {
		return fmt.Errorf("-execute requires -manifest")
	}
	u, err := url.Parse(*endpoint)
	if err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") || u.Path != "/mcp/memory" || u.User != nil || u.RawQuery != "" {
		return fmt.Errorf("URL must be a loopback broker /mcp/memory endpoint")
	}
	token := os.Getenv("BROKER_TOKEN")
	if token == "" {
		return fmt.Errorf("BROKER_TOKEN must contain an owner token (never pass it in argv)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "brokerd-memory-cleanup", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: *endpoint, MaxRetries: -1, DisableStandaloneSSE: true, HTTPClient: &http.Client{Transport: bearer(token), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil)
	if err != nil {
		return fmt.Errorf("could not connect to broker memory front")
	}
	defer session.Close()
	var m memorycleanup.Manifest
	if *manifest == "" {
		m, err = memorycleanup.Inventory(ctx, session)
	} else {
		var raw []byte
		raw, err = os.ReadFile(*manifest)
		if err == nil {
			err = json.Unmarshal(raw, &m)
		}
		if err == nil {
			err = memorycleanup.Apply(ctx, session, m, *execute)
		}
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	fmt.Fprintf(os.Stderr, "dry_run=%v memories=%d\n", !*execute, len(m.Memories))
	return encoder.Encode(m)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
