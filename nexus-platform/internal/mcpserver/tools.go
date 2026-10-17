package mcpserver

import (
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"nexus/platform/internal/ai"
)

// toolPrefix names every tool this server exposes: nexus_<the builder agent's tool name>.
const toolPrefix = "nexus_"

// readPrefixes are the verbs of the builder agent's read tools. A tool is exposed only if its name
// starts with one of these or with "propose_": a tool with any other verb (apply, publish, write…)
// never reaches an outside agent, even if the builder agent gains one.
var readPrefixes = []string{"list_", "get_", "snapshot_", "explain_"}

func isPropose(name string) bool { return strings.HasPrefix(name, "propose_") }

func isRead(name string) bool {
	for _, p := range readPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// no backs the hints that are false for every tool: none destroys anything (a proposal only adds
// a card) and none reaches outside the workspace. Shared and never written.
var no = false

// proposeNote ends every propose tool's description: what the outside agent must tell its user.
const proposeNote = " MCP: the proposal is saved as a card in the builder's Studio thread; nothing changes until a human applies it to drafts and publishes it."

// tools turns the builder agent's tool definitions (ai.ToolDefinitions) into MCP tools: every read
// tool and every propose_* tool, renamed nexus_<name>, with the same description and JSON schema,
// so a tool the builder agent gains appears here with no change to this package.
func tools(defs []ai.Tool) []*mcp.Tool {
	out := make([]*mcp.Tool, 0, len(defs))
	for _, d := range defs {
		name := d.Function.Name
		read, propose := isRead(name), isPropose(name)
		if !read && !propose {
			continue
		}
		t := &mcp.Tool{
			Name:        toolPrefix + name,
			Title:       title(name),
			Description: d.Function.Description,
			InputSchema: d.Function.Parameters,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: read, DestructiveHint: &no, OpenWorldHint: &no},
		}
		if propose {
			t.Description += proposeNote
		}
		out = append(out, t)
	}
	return out
}

// title is the human-readable name of a tool: "propose_entity" → "Propose entity".
func title(name string) string {
	s := strings.ReplaceAll(name, "_", " ")
	return strings.ToUpper(s[:1]) + s[1:]
}
