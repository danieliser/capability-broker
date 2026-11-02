// brokerd-probe is a tiny MCP client for exercising a running broker from the
// shell: list tools, or call one tool with JSON arguments, using a bearer token.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b *bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8890/mcp/clickhouse", "connector endpoint")
	token := flag.String("token", os.Getenv("BROKER_TOKEN"), "bearer token (or BROKER_TOKEN)")
	tool := flag.String("tool", "", "tool to call; empty lists tools")
	args := flag.String("args", "{}", "JSON arguments")
	raw := flag.Bool("raw", false, "print the server instructions, tool definitions and results as raw JSON")
	flag.Parse()
	if *token == "" {
		fmt.Fprintln(os.Stderr, "token required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "brokerd-probe", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: *url, DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: &bearer{token: *token, next: http.DefaultTransport}}}, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
		os.Exit(1)
	}
	defer session.Close()
	if *raw && *tool == "" {
		if ir := session.InitializeResult(); ir != nil {
			fmt.Printf("instructions: %q\n", ir.Instructions)
		}
		res, err := session.ListTools(ctx, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tools/list failed: %v\n", err)
			os.Exit(1)
		}
		for _, t := range res.Tools {
			b, _ := json.Marshal(t)
			fmt.Println(string(b))
		}
		return
	}
	if *tool == "" {
		res, err := session.ListTools(ctx, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tools/list failed: %v\n", err)
			os.Exit(1)
		}
		for _, t := range res.Tools {
			schema, _ := json.Marshal(t.InputSchema)
			fmt.Printf("%s\n  %s\n  schema: %s\n", t.Name, strings.ReplaceAll(t.Description, "\n", " "), string(schema))
		}
		return
	}
	var a map[string]any
	if err := json.Unmarshal([]byte(*args), &a); err != nil {
		fmt.Fprintf(os.Stderr, "bad args: %v\n", err)
		os.Exit(2)
	}
	started := time.Now()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: *tool, Arguments: a})
	if err != nil {
		fmt.Fprintf(os.Stderr, "tools/call failed: %v\n", err)
		os.Exit(1)
	}
	if *raw {
		b, _ := json.Marshal(res)
		fmt.Println(string(b))
	} else {
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				fmt.Println(tc.Text)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "isError=%v elapsed=%s\n", res.IsError, time.Since(started).Round(time.Millisecond))
	if res.IsError {
		os.Exit(3)
	}
}
