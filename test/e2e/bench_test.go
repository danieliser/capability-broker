package e2e

import (
	"context"
	"os/exec"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// BenchmarkDirect calls the mock server's echo tool over its own stdio session, as a client would without the broker.
func BenchmarkDirect(b *testing.B) {
	c := mcp.NewClient(&mcp.Implementation{Name: "bench", Version: "0"}, nil)
	s, err := c.Connect(context.Background(), &mcp.CommandTransport{Command: exec.Command(mockBin)}, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	args := map[string]any{"text": "hello"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: args}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBroker makes the same call through the broker front (sessionless Streamable HTTP, owner token,
// grant check, receipts) to the same kind of child.
func BenchmarkBroker(b *testing.B) {
	e := newEnv(b, mock("mock"))
	s := e.connect(b, e.owner(b, "claude-code"), "/mcp/mock", nil)
	args := map[string]any{"text": "hello"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: args}); err != nil {
			b.Fatal(err)
		}
	}
}
