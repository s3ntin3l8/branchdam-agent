package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadArgsSidecarParsing(t *testing.T) {
	tmpDir := t.TempDir()
	sidecarPath := filepath.Join(tmpDir, "args.json")

	args := []string{"tray", "-config", "/etc/config.yaml"}
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var parsedArgs []string
	parsedData, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(parsedData, &parsedArgs); err != nil {
		t.Fatal(err)
	}
	if len(parsedArgs) != 3 {
		t.Errorf("got %d args, want 3", len(parsedArgs))
	}
	if parsedArgs[0] != "tray" {
		t.Errorf("got arg[0] %q, want %q", parsedArgs[0], "tray")
	}
}

func TestReadArgsMissingFile(t *testing.T) {
	_, err := os.ReadFile("/nonexistent/path/args.json")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestReadArgsInvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	sidecarPath := filepath.Join(tmpDir, "args.json")

	if err := os.WriteFile(sidecarPath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsedArgs []string
	if err := json.Unmarshal(data, &parsedArgs); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestReadArgsSidecarWithSpecialChars(t *testing.T) {
	tmpDir := t.TempDir()
	sidecarPath := filepath.Join(tmpDir, "args.json")

	args := []string{"-config", "C:\\Users\\test;calc.exe", "-other", "arg>with>special"}
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var parsedArgs []string
	parsedData, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(parsedData, &parsedArgs); err != nil {
		t.Fatal(err)
	}

	if parsedArgs[1] != args[1] {
		t.Errorf("arg[1] = %q, want %q", parsedArgs[1], args[1])
	}
	if parsedArgs[3] != args[3] {
		t.Errorf("arg[3] = %q, want %q", parsedArgs[3], args[3])
	}
}
