// One SDK client and HTTP transport for both daemons; sequential owner calls.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/durable"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}
func must(e error) {
	if e != nil {
		panic(e)
	}
}
func encode(v any) { must(json.NewEncoder(os.Stdout).Encode(v)) }
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if os.Args[1] == "seed" {
		root := os.Args[2]
		db, e := durable.Open(ctx, filepath.Join(root, "broker.db"))
		must(e)
		defer db.Close()
		_, token, e := auth.NewStore(db).Mint(ctx, auth.MintRequest{Kind: auth.KindClient, Name: "rust-comparison", Capabilities: auth.OwnerCapabilities(), IssuedBy: "isolated benchmark"})
		must(e)
		must(os.WriteFile(filepath.Join(root, "owner.token"), []byte(token), 0600))
		return
	}
	token, e := os.ReadFile(os.Args[3])
	must(e)
	client := mcp.NewClient(&mcp.Implementation{Name: "comparison", Version: "1"}, nil)
	s, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: os.Args[2], MaxRetries: -1, HTTPClient: &http.Client{Transport: bearer(strings.TrimSpace(string(token)))}}, nil)
	must(e)
	defer s.Close()
	list, e := s.ListTools(ctx, nil)
	must(e)
	if os.Args[1] == "ready" {
		if len(list.Tools) == 0 {
			panic("empty tools")
		}
		encode(map[string]any{"tools": len(list.Tools)})
		return
	}
	if os.Args[1] == "smoke" {
		direct, e := client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command(os.Args[4])}, nil)
		must(e)
		defer direct.Close()
		dl, e := direct.ListTools(ctx, nil)
		must(e)
		a, _ := json.Marshal(list.Tools)
		b, _ := json.Marshal(dl.Tools)
		if string(a) != string(b) {
			panic(fmt.Sprintf("tool definitions differ: %s vs %s", a, b))
		}
		for _, name := range []string{"echo", "markdown", "pixel", "fail", "noisy"} {
			p := &mcp.CallToolParams{Name: name, Arguments: map[string]any{"text": "hello"}}
			got, e := s.CallTool(ctx, p)
			must(e)
			want, e := direct.CallTool(ctx, p)
			must(e)
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(want)
			var av, bv any
			must(json.Unmarshal(a, &av))
			must(json.Unmarshal(b, &bv))
			if !reflect.DeepEqual(av, bv) {
				panic(fmt.Sprintf("%s result differs: %s vs %s", name, a, b))
			}
		}
		encode(map[string]any{"verbatim_tools": true, "verbatim_results": []string{"echo", "markdown", "pixel", "fail", "noisy"}})
		return
	}
	var runs []any
	for run := 0; run < 3; run++ {
		samples := make([]int64, 2000)
		start := time.Now()
		for i := range samples {
			t := time.Now()
			r, e := s.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hello"}})
			must(e)
			if r.IsError {
				panic("tool error")
			}
			samples[i] = time.Since(t).Nanoseconds()
		}
		elapsed := time.Since(start)
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		runs = append(runs, map[string]any{"median_us": float64(samples[999]+samples[1000]) / 2000, "mean_us": float64(elapsed.Nanoseconds()) / 2000000, "calls": 2000})
		// The runner samples RSS on this process boundary, after exactly 2,000 calls.
		if run == 0 {
			fmt.Fprintln(os.Stderr, "RSS_READY")
			time.Sleep(300 * time.Millisecond)
		}
	}
	encode(runs)
}
