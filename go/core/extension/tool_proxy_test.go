package extension

import (
	"encoding/json"
	"testing"

	core "github.com/SmooAI/smooth-operator-core/go/core"
)

// Compile-time proof that an ExtensionTool is a core.Tool — so the server can
// append host.Tools() straight into core.AgentOptions.Tools.
var _ core.Tool = (*ExtensionTool)(nil)

func TestExtensionToolSchemaUsesDottedName(t *testing.T) {
	reg := ToolRegistration{
		Name:        "say",
		Description: "Echo a phrase back.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"phrase":{"type":"string"}},"required":["phrase"]}`),
	}
	// Name/Description/Parameters never touch the process, so nil is fine here.
	tool := NewExtensionTool("echo", reg, nil, Context{Token: "epoch-1", Tier: TierCommand})
	if tool.Name() != "echo.say" {
		t.Errorf("name = %q", tool.Name())
	}
	if tool.Description() != "Echo a phrase back." {
		t.Errorf("description = %q", tool.Description())
	}
	params := tool.Parameters()
	if params["type"] != "object" {
		t.Errorf("parameters = %+v", params)
	}
	if tool.IsConcurrentSafe() {
		t.Error("extension tools are not concurrent-safe by default")
	}
}
