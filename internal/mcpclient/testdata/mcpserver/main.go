// Command mcpserver is the MCP stdio test fixture for internal/mcpclient:
// it serves one `echo` tool so the E2E test can drive a REAL server
// subprocess through the SDK's stdio transport. Built by the test on the
// fly (`go build ./internal/mcpclient/testdata/mcpserver`).
package main

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoArgs struct {
	Text string `json:"text" jsonschema:"the text to echo back"`
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "xdev-test-mcp", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "echo",
		Description: "echo the given text back",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args echoArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + args.Text}},
		}, nil, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
