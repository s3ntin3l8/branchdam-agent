package protocolhandler

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommandNamesDeepLinkFlagAndQuotesPath(t *testing.T) {
	got := Command(`C:\Program Files\branchDAM\branchdam-agent-tray.exe`)
	want := `"C:\Program Files\branchDAM\branchdam-agent-tray.exe" pair -deeplink "%1"`
	if got != want {
		t.Errorf("Command = %s, want %s", got, want)
	}
}

func TestHandlerExePrefersTraySibling(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "branchdam-agent.exe")
	if got := HandlerExe(self); got != self {
		t.Errorf("no tray sibling: HandlerExe = %s, want self", got)
	}
	tray := filepath.Join(dir, "branchdam-agent-tray.exe")
	if err := os.WriteFile(tray, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := HandlerExe(self); got != tray {
		t.Errorf("tray sibling present: HandlerExe = %s, want %s", got, tray)
	}
}
