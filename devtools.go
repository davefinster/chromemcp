package main

// Passthrough to Google's chrome-devtools-mcp: one stdio instance per
// session, attached to that session's Chrome over its DevTools port, started
// on first use and stopped with the session. Its tools are surfaced as a
// pair — list and call — rather than re-registered one by one, so the
// tool list of this server stays stable across its releases and an agent
// reads the live schemas when it needs them.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type devtoolsClient struct {
	cmdline string
	port    int
	verbose bool

	mu      sync.Mutex
	session *mcp.ClientSession
	cmd     *exec.Cmd
	tools   []*mcp.Tool
}

// splitCommand for the passthrough: the configured command line is a
// program and its arguments, space-separated (no quoting needed for the
// forms this takes: a path, or "npx -y chrome-devtools-mcp@x.y.z").
func newDevToolsClient(cmdline string, port int, verbose bool) *devtoolsClient {
	return &devtoolsClient{cmdline: cmdline, port: port, verbose: verbose}
}

// connect starts the subprocess if it is not running. Caller holds dc.mu.
func (dc *devtoolsClient) connect(ctx context.Context) error {
	if dc.session != nil {
		return nil
	}
	parts := strings.Fields(dc.cmdline)
	if len(parts) == 0 {
		return fmt.Errorf("no chrome-devtools-mcp command configured")
	}
	args := append(parts[1:],
		"--browserUrl", fmt.Sprintf("http://127.0.0.1:%d", dc.port),
		"--no-usage-statistics",
		"--screenshotFormat", "jpeg",
		"--screenshotQuality", "70",
	)
	cmd := exec.Command(parts[0], args...)
	cmd.Env = append(os.Environ(), "CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS=1")
	if dc.verbose {
		cmd.Stderr = os.Stderr
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "chromemcp", Version: version}, nil)
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second) // npx may have to fetch it
	defer cancel()
	sess, err := client.Connect(cctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("starting chrome-devtools-mcp (%s): %w", dc.cmdline, err)
	}
	res, err := sess.ListTools(cctx, nil)
	if err != nil {
		sess.Close()
		return fmt.Errorf("listing chrome-devtools-mcp tools: %w", err)
	}
	dc.session, dc.cmd, dc.tools = sess, cmd, res.Tools
	return nil
}

func (dc *devtoolsClient) listTools(ctx context.Context) ([]*mcp.Tool, error) {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if err := dc.connect(ctx); err != nil {
		return nil, err
	}
	return dc.tools, nil
}

// call forwards one tool call. A page-scoped tool called without a pageId
// is routed to the page at currentURL (this server's current tab). A dead
// subprocess is restarted once.
func (dc *devtoolsClient) call(ctx context.Context, name string, args map[string]any, currentURL string) (*mcp.CallToolResult, error) {
	if args == nil {
		args = map[string]any{} // the server wants an object, not nothing
	}
	dc.mu.Lock()
	defer dc.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if err := dc.connect(ctx); err != nil {
			return nil, err
		}
		if _, given := args["pageId"]; !given && currentURL != "" && dc.wantsPageID(name) {
			if id, ok := dc.pageIDFor(ctx, currentURL); ok {
				args["pageId"] = id
			}
		}
		res, err := dc.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		// Connection-level failure: drop the session and try a fresh one.
		if strings.Contains(err.Error(), "closed") || strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "broken pipe") {
			dc.reset()
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("chrome-devtools-mcp did not answer")
}

func (dc *devtoolsClient) reset() {
	if dc.session != nil {
		dc.session.Close()
	}
	if dc.cmd != nil && dc.cmd.Process != nil {
		dc.cmd.Process.Kill()
	}
	dc.session, dc.cmd, dc.tools = nil, nil, nil
}

func (dc *devtoolsClient) close() {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.reset()
}

// wantsPageID reports whether a tool's input schema has a pageId property
// (chrome-devtools-mcp's page-scoped tools, with its page-id routing on).
func (dc *devtoolsClient) wantsPageID(name string) bool {
	for _, t := range dc.tools {
		if t.Name != name || t.InputSchema == nil {
			continue
		}
		b, err := json.Marshal(t.InputSchema)
		if err != nil {
			return false
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if json.Unmarshal(b, &schema) != nil {
			return false
		}
		_, ok := schema.Properties["pageId"]
		return ok
	}
	return false
}

var pageLineRe = regexp.MustCompile(`(?m)^(\d+): .*\((\S+)\)`)

// pageIDFor asks chrome-devtools-mcp for its page list and returns the id
// of the page at url, so a call can be routed to this server's current tab.
// Caller holds dc.mu.
func (dc *devtoolsClient) pageIDFor(ctx context.Context, url string) (int, bool) {
	res, err := dc.session.CallTool(ctx, &mcp.CallToolParams{Name: "list_pages", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		return 0, false
	}
	for _, c := range res.Content {
		tc, ok := c.(*mcp.TextContent)
		if !ok {
			continue
		}
		for _, m := range pageLineRe.FindAllStringSubmatch(tc.Text, -1) {
			if m[2] == url {
				n, err := strconv.Atoi(m[1])
				return n, err == nil
			}
		}
	}
	return 0, false
}

// describeTools renders the passthrough's tool list compactly.
func describeTools(tools []*mcp.Tool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d tools from chrome-devtools-mcp; call them with devtools_call(session_id, tool, arguments).\n\n", len(tools))
	for _, t := range tools {
		fmt.Fprintf(&sb, "## %s\n%s\n", t.Name, strings.TrimSpace(t.Description))
		if t.InputSchema != nil {
			if b, err := json.Marshal(t.InputSchema); err == nil && string(b) != "{}" {
				fmt.Fprintf(&sb, "input schema: %s\n", b)
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
