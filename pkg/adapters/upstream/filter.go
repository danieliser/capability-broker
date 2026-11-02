package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/compact"
	"github.com/danieliser/capability-broker/pkg/connectors"
)

// filterResult applies a connector's response filter: drop_keys, compact, command, max_chars, in that order.
// JSON text blocks and structuredContent are reshaped as JSON; other text is left to command and max_chars.
func filterResult(ctx context.Context, res *mcp.CallToolResult, f *connectors.ResponseFilter) (*mcp.CallToolResult, error) {
	out := *res
	out.Content = append([]mcp.Content{}, res.Content...)
	reshape := func(v any) any {
		if len(f.DropKeys) > 0 {
			v = dropKeys(v, f.DropKeys)
		}
		if f.Compact {
			v = compact.Trim(v, compact.Options{MaxString: f.MaxString, MaxItems: f.MaxItems, DropKeys: f.DropKeys})
		}
		return v
	}
	if len(f.DropKeys) > 0 || f.Compact {
		for i, c := range out.Content {
			tc, ok := c.(*mcp.TextContent)
			if !ok {
				continue
			}
			var v any
			if json.Unmarshal([]byte(tc.Text), &v) != nil {
				continue
			}
			b, _ := json.Marshal(reshape(v))
			out.Content[i] = &mcp.TextContent{Text: string(b), Annotations: tc.Annotations, Meta: tc.Meta}
		}
		if out.StructuredContent != nil {
			out.StructuredContent = reshape(decodeAny(out.StructuredContent))
		}
	}
	if len(f.Command) > 0 {
		in, _ := json.Marshal(&out)
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, f.Command[0], f.Command[1:]...)
		cmd.Stdin = bytes.NewReader(in)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("%s: %v %s", f.Command[0], err, strings.TrimSpace(stderr.String()))
		}
		text := strings.TrimSpace(stdout.String())
		var replaced mcp.CallToolResult
		if strings.HasPrefix(text, "{") && json.Unmarshal([]byte(text), &replaced) == nil && replaced.Content != nil {
			out = replaced
		} else {
			out = mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: res.IsError}
		}
	}
	if f.MaxChars > 0 {
		for i, c := range out.Content {
			if tc, ok := c.(*mcp.TextContent); ok && len([]rune(tc.Text)) > f.MaxChars {
				r := []rune(tc.Text)
				out.Content[i] = &mcp.TextContent{Text: string(r[:f.MaxChars]) + fmt.Sprintf("\n…[%d more characters cut by the broker]", len(r)-f.MaxChars),
					Annotations: tc.Annotations, Meta: tc.Meta}
			}
		}
	}
	return &out, nil
}

func decodeAny(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return v
	}
	return out
}

func dropKeys(v any, keys []string) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			drop := false
			for _, d := range keys {
				if k == d {
					drop = true
				}
			}
			if !drop {
				m[k] = dropKeys(x, keys)
			}
		}
		return m
	case []any:
		a := make([]any, len(t))
		for i, x := range t {
			a[i] = dropKeys(x, keys)
		}
		return a
	}
	return v
}
