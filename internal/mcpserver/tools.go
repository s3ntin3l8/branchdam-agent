package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

const (
	defaultFailedLimit = 50
	maxFailedLimit     = 500
)

type failedIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum rows to return, from 1 to 500; omitted means 50"`
}

type lookupIn struct {
	NodeUUID   string `json:"nodeUuid,omitempty" jsonschema:"queue row node uuid; set exactly one lookup key"`
	SourcePath string `json:"sourcePath,omitempty" jsonschema:"card path; set exactly one lookup key"`
	LocalPath  string `json:"localPath,omitempty" jsonschema:"local edit-copy path; set exactly one lookup key"`
}

type countsOut struct {
	AwaitingUpload int   `json:"awaitingUpload"`
	AwaitingRebase int   `json:"awaitingRebase"`
	Failed         int   `json:"failed"`
	Done           int   `json:"done"`
	Pending        int   `json:"pending"`
	PendingBytes   int64 `json:"pendingBytes"`
}

type failedOut struct {
	Rows []FailedRow `json:"rows"`
}

type lookupOut struct {
	Found bool       `json:"found"`
	Row   *FailedRow `json:"row,omitempty"`
}

type preflightOut struct {
	OK     bool          `json:"ok"`
	Checks []CheckResult `json:"checks"`
}

func registerTools(s *sdkmcp.Server, opts Options) {
	sdkmcp.AddTool(s, readOnlyTool("queue_summary", "Count rows in the local offline queue (awaiting upload, awaiting rebase, failed, done)."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, countsOut, error) {
			store, err := opts.openQueue()
			if err != nil {
				return nil, countsOut{}, err
			}
			defer func() { _ = store.Close() }()
			c, err := store.Counts(ctx)
			if err != nil {
				return nil, countsOut{}, err
			}
			out := countsOut{
				AwaitingUpload: c.AwaitingUpload,
				AwaitingRebase: c.AwaitingRebase,
				Failed:         c.Failed,
				Done:           c.Done,
				Pending:        c.Pending(),
				PendingBytes:   c.PendingBytes,
			}
			return textResult(countSummary(c)), out, nil
		})

	sdkmcp.AddTool(s, readOnlyTool("list_failed", "List queue rows whose archive copy, rebase, or upload failed terminally."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in failedIn) (*sdkmcp.CallToolResult, failedOut, error) {
			limit, err := failedLimit(in.Limit)
			if err != nil {
				return nil, failedOut{}, err
			}
			store, err := opts.openQueue()
			if err != nil {
				return nil, failedOut{}, err
			}
			defer func() { _ = store.Close() }()
			recs, err := store.FailedRows(ctx, limit)
			if err != nil {
				return nil, failedOut{}, err
			}
			rows := make([]FailedRow, 0, len(recs))
			for _, rec := range recs {
				rows = append(rows, failedRow(rec))
			}
			return textResult(fmt.Sprintf("%d failed", len(rows))), failedOut{Rows: rows}, nil
		})

	sdkmcp.AddTool(s, readOnlyTool("lookup_queue_row", "Look up one queue row by exactly one of nodeUuid, sourcePath, or localPath."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in lookupIn) (*sdkmcp.CallToolResult, lookupOut, error) {
			rec, ok, err := lookup(ctx, opts, in)
			if err != nil {
				return nil, lookupOut{}, err
			}
			if !ok {
				return textResult("not found"), lookupOut{Found: false}, nil
			}
			row := failedRow(rec)
			if !rec.Failed() {
				row.Step = ""
				row.LastError = ""
				row.Attempts = 0
			}
			return textResult("found " + rec.NodeUUID), lookupOut{Found: true, Row: &row}, nil
		})

	sdkmcp.AddTool(s, readOnlyTool("agent_config", "Return the loaded agent config. server.apiKey is redacted. An unexpanded ${VAR} placeholder is shown as the placeholder, not the environment value."),
		func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, map[string]any, error) {
			if opts.ConfigPath == "" {
				return nil, nil, errors.New("config path is not set")
			}
			cfg, err := redactedConfig(opts.ConfigPath)
			if err != nil {
				return nil, nil, err
			}
			return textResult("config loaded"), cfg, nil
		})

	sdkmcp.AddTool(s, readOnlyTool("preflight", "Run the same checks as the preflight subcommand and return them as structured results."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, preflightOut, error) {
			if opts.Preflight == nil {
				return nil, preflightOut{}, errors.New("preflight is not configured")
			}
			checks, ok, err := opts.Preflight(ctx)
			if err != nil {
				return nil, preflightOut{}, err
			}
			summary := "preflight passed"
			if !ok {
				summary = "preflight failed"
			}
			return textResult(summary), preflightOut{OK: ok, Checks: checks}, nil
		})
}

func failedLimit(limit int) (int, error) {
	if limit == 0 {
		return defaultFailedLimit, nil
	}
	if limit < 1 || limit > maxFailedLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", maxFailedLimit)
	}
	return limit, nil
}

func lookup(ctx context.Context, opts Options, in lookupIn) (queue.Record, bool, error) {
	keys := 0
	if strings.TrimSpace(in.NodeUUID) != "" {
		keys++
	}
	if strings.TrimSpace(in.SourcePath) != "" {
		keys++
	}
	if strings.TrimSpace(in.LocalPath) != "" {
		keys++
	}
	if keys != 1 {
		return queue.Record{}, false, errors.New("lookup_queue_row requires exactly one of nodeUuid, sourcePath, localPath")
	}
	store, err := opts.openQueue()
	if err != nil {
		return queue.Record{}, false, err
	}
	defer func() { _ = store.Close() }()
	switch {
	case strings.TrimSpace(in.NodeUUID) != "":
		return store.ByNodeUUID(ctx, strings.TrimSpace(in.NodeUUID))
	case strings.TrimSpace(in.SourcePath) != "":
		return store.BySourcePath(ctx, strings.TrimSpace(in.SourcePath))
	default:
		return store.ByLocalPath(ctx, strings.TrimSpace(in.LocalPath))
	}
}
