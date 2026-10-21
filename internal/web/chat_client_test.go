package web

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatClientLifecycle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is optional for the deterministic client lifecycle check")
	}
	command := exec.CommandContext(t.Context(), node, "testdata/chat_lifecycle.cjs", "static/app.js")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
