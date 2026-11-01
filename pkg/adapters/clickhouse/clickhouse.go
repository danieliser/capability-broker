// Package clickhouse is the ClickHouse adapter: named parameterised queries and,
// for grants with sql.select, guarded ad-hoc read statements, over the HTTP
// interface as a dedicated read-only user. Limits live in that user's settings
// profile; the adapter adds client-side row truncation to the grant's max_rows.
package clickhouse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/danieliser/capability-broker/pkg/adapter"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/config"
)

const AdapterVersion = "1.0"

type Adapter struct {
	cfg      config.ClickHouse
	password string
	cat      *catalog.Catalog
	client   *http.Client
}

func New(cfg config.ClickHouse, password string, cat *catalog.Catalog) *Adapter {
	return &Adapter{cfg: cfg, password: password, cat: cat, client: &http.Client{
		Timeout: cfg.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("redirects are refused")
		},
	}}
}

func (a *Adapter) Connector() string { return "clickhouse" }

// Tools: agent grants without sql.select see only run_query, with the named
// queries they may call listed in the description so no discovery call is needed.
func (a *Adapter) Tools(_ context.Context, g *auth.Grant) ([]catalog.Tool, error) {
	conn, _ := a.cat.Connector("clickhouse")
	cap, _ := g.Capabilities.For("clickhouse")
	owner := g.Capabilities.IsOwner()
	var out []catalog.Tool
	for _, t := range conn.Tools {
		if !owner && cap.SQL != "select" && t.Name != "run_query" {
			continue
		}
		tool := t
		if t.Name == "run_query" && !owner {
			tool.Description = a.describeForGrant(t.Description, cap)
		}
		out = append(out, tool)
	}
	return out, nil
}

func (a *Adapter) describeForGrant(base string, cap auth.ConnectorCapability) string {
	var b strings.Builder
	if cap.SQL != "select" {
		b.WriteString("Run one of this grant's named queries by passing query=\"@id\" and params. Ad-hoc SQL is not permitted for this grant. ")
	} else {
		b.WriteString(base)
		b.WriteString(" ")
	}
	qs := a.cat.Queries(cap.Queries)
	if len(qs) > 0 {
		b.WriteString("Named queries: ")
		for i, q := range qs {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString("@" + q.ID)
			if len(q.Params) > 0 {
				b.WriteString("(")
				first := true
				for name, p := range q.Params {
					if !first {
						b.WriteString(", ")
					}
					first = false
					b.WriteString(name + ":" + p.Type)
					if p.Required {
						b.WriteString("!")
					}
				}
				b.WriteString(")")
			}
			if q.Summary != "" {
				b.WriteString(" — " + q.Summary)
			}
		}
		b.WriteString(".")
	}
	return b.String()
}

func (a *Adapter) Health(ctx context.Context) error {
	if a.password == "" {
		return fmt.Errorf("clickhouse password not configured")
	}
	_, _, err := a.query(ctx, "SELECT 1", nil, 0)
	return err
}

var readOnlyStart = regexp.MustCompile(`(?is)^\s*(SELECT|WITH|SHOW|DESCRIBE|DESC|EXPLAIN|EXISTS)\b`)
var formatClause = regexp.MustCompile(`(?is)\bFORMAT\s+[A-Za-z0-9]+\s*;?\s*$`)

// guardSQL applies the catalog's guards for ad-hoc statements.
func guardSQL(sql string, guards []string) error {
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql), ";"))
	for _, g := range guards {
		switch g {
		case "single_statement":
			if strings.Contains(trimmed, ";") {
				return adapter.Denied("one statement per call")
			}
		case "select_only":
			if !readOnlyStart.MatchString(trimmed) {
				return adapter.Denied("only SELECT, WITH, SHOW, DESCRIBE, EXPLAIN and EXISTS statements are permitted")
			}
			upper := strings.ToUpper(trimmed)
			if strings.Contains(upper, "INTO OUTFILE") || regexp.MustCompile(`\bSETTINGS\b`).MatchString(upper) {
				return adapter.Denied("INTO OUTFILE and SETTINGS clauses are not permitted")
			}
		}
	}
	return nil
}

type compactResponse struct {
	QueryID   string   `json:"query_id"`
	Columns   []string `json:"columns"`
	Rows      int      `json:"rows"`
	Truncated bool     `json:"truncated,omitempty"`
	Preview   [][]any  `json:"preview"`
	ElapsedMs int64    `json:"elapsed_ms"`
	ReadRows  string   `json:"read_rows,omitempty"`
	ReadBytes string   `json:"read_bytes,omitempty"`
}

type jsonCompact struct {
	Meta []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"meta"`
	Data       [][]any        `json:"data"`
	Rows       int            `json:"rows"`
	Statistics map[string]any `json:"statistics"`
	Exception  string         `json:"exception"`
}

func (a *Adapter) Execute(ctx context.Context, call adapter.Call) (*adapter.Result, error) {
	args := call.Decision.Args
	maxRows := 0
	if v, ok := call.Decision.Limits["max_rows"].(float64); ok {
		maxRows = int(v)
	}
	var sql string
	params := map[string]string{}
	switch call.Op.ID {
	case "clickhouse.named_query":
		q := call.Decision.Query
		if q == nil {
			return nil, adapter.Denied("named query not resolved")
		}
		sql = q.SQL
		given := adapter.ArgObject(args, "params")
		for name, p := range q.Params {
			v, ok := given[name]
			if !ok || v == nil {
				if p.Required {
					return nil, adapter.Denied(fmt.Sprintf("param %s is required", name))
				}
				if p.Default != "" {
					params[name] = p.Default
				}
				continue
			}
			params[name] = formatParam(v)
		}
		for name := range given {
			if _, declared := q.Params[name]; !declared {
				return nil, adapter.Denied(fmt.Sprintf("param %s is not declared by query %s", name, q.ID))
			}
		}
	case "clickhouse.sql.select":
		sql = adapter.ArgString(args, "query")
		if err := guardSQL(sql, call.Op.Guards); err != nil {
			return nil, err
		}
		for name, v := range adapter.ArgObject(args, "params") {
			if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(name) {
				return nil, adapter.Denied("invalid param name " + name)
			}
			params[name] = formatParam(v)
		}
	case "clickhouse.schema.databases":
		sql = "SELECT name FROM system.databases ORDER BY name"
	case "clickhouse.schema.tables":
		db := adapter.ArgString(args, "database")
		if db == "" {
			return nil, adapter.Denied("database is required")
		}
		params["db"] = db
		sql = "SELECT name, engine, total_rows, comment FROM system.tables WHERE database = {db:String}"
		if like := adapter.ArgString(args, "like"); like != "" {
			params["like"] = like
			sql += " AND name LIKE {like:String}"
		}
		if nl := adapter.ArgString(args, "not_like"); nl != "" {
			params["not_like"] = nl
			sql += " AND name NOT LIKE {not_like:String}"
		}
		sql += " ORDER BY name"
	default:
		return nil, adapter.Denied("unknown operation " + call.Op.ID)
	}

	started := time.Now()
	res, queryID, err := a.query(ctx, sql, params, maxRows)
	if err != nil {
		return nil, err
	}
	cols := make([]string, 0, len(res.Meta))
	for _, m := range res.Meta {
		cols = append(cols, m.Name)
	}
	truncated := false
	data := res.Data
	if maxRows > 0 && len(data) > maxRows {
		data = data[:maxRows]
		truncated = true
	}
	previewN := adapter.IntFromCompact(call.Op, "rows_preview", 20)
	preview := data
	if len(preview) > previewN {
		preview = preview[:previewN]
	}
	compact := compactResponse{QueryID: queryID, Columns: cols, Rows: len(data), Truncated: truncated || len(data) > len(preview),
		Preview: preview, ElapsedMs: time.Since(started).Milliseconds()}
	compact.ReadRows = statString(res.Statistics["rows_read"])
	compact.ReadBytes = statString(res.Statistics["bytes_read"])
	verbose := map[string]any{"query_id": queryID, "meta": res.Meta, "data": data, "rows": len(data), "truncated": truncated, "statistics": res.Statistics}
	return &adapter.Result{Compact: compact, Verbose: verbose, Outcome: adapter.OutcomeCompleted, UpstreamRequestID: queryID,
		OutcomeClass: "ok", Account: a.cfg.User}, nil
}

// statString renders a JSONCompact statistic (decoded as float64) as an integer string.
func statString(v any) string {
	switch t := v.(type) {
	case float64:
		return strconv.FormatFloat(t, 'f', 0, 64)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

// formatParam renders a JSON value as a ClickHouse HTTP parameter value.
func formatParam(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case bool:
		if t {
			return "1"
		}
		return "0"
	case nil:
		return "\\N"
	case []any:
		parts := make([]string, 0, len(t))
		for _, x := range t {
			switch xs := x.(type) {
			case string:
				parts = append(parts, "'"+strings.ReplaceAll(strings.ReplaceAll(xs, "\\", "\\\\"), "'", "\\'")+"'")
			default:
				parts = append(parts, formatParam(x))
			}
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// query posts the statement with bound params. No settings travel in the URL:
// the read-only user's profile owns readonly, max_result_rows and max_execution_time.
func (a *Adapter) query(ctx context.Context, sql string, params map[string]string, maxRows int) (*jsonCompact, string, error) {
	if a.password == "" {
		return nil, "", adapter.Unavailable("clickhouse credential is not configured on the broker")
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql), ";"))
	if !formatClause.MatchString(body) {
		body += " FORMAT JSONCompact"
	} else {
		body = formatClause.ReplaceAllString(body, " FORMAT JSONCompact")
	}
	q := url.Values{}
	for k, v := range params {
		q.Set("param_"+k, v)
	}
	if a.cfg.Database != "" {
		q.Set("database", a.cfg.Database)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.cfg.URL, "/")+"/?"+q.Encode(), bytes.NewBufferString(body))
	if err != nil {
		return nil, "", adapter.Failed(err.Error())
	}
	req.SetBasicAuth(a.cfg.User, a.password)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, "", adapter.Unavailable(redact(err.Error(), a.password))
	}
	defer resp.Body.Close()
	queryID := resp.Header.Get("X-ClickHouse-Query-Id")
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, queryID, &adapter.UpstreamError{Code: adapter.CodeUpstreamError, Message: "response read failed", Outcome: adapter.OutcomeUnknown}
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if code := resp.Header.Get("X-ClickHouse-Exception-Code"); code != "" {
			msg = "exception " + code + ": " + msg
		}
		return nil, queryID, adapter.Failed(redact(msg, a.password))
	}
	var out jsonCompact
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, queryID, adapter.Failed("clickhouse returned a non-JSON body: " + adapter.Truncate(redact(string(raw), a.password), 200))
	}
	if out.Exception != "" {
		return nil, queryID, adapter.Failed(redact(out.Exception, a.password))
	}
	return &out, queryID, nil
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

var _ adapter.Adapter = (*Adapter)(nil)
