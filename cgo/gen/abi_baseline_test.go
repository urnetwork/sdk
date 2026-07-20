package main

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExportedSymbolCompatibilityBaseline(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve test path")
	}
	genDir := filepath.Dir(filename)
	baselineFile, err := os.Open(filepath.Join(genDir, "testdata", "exported_symbols.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer baselineFile.Close()
	defBytes, err := os.ReadFile(filepath.Join(genDir, "..", "include", "urnetwork_sdk.def"))
	if err != nil {
		t.Fatal(err)
	}
	exports := "\n" + string(defBytes) + "\n"
	scanner := bufio.NewScanner(baselineFile)
	for scanner.Scan() {
		symbol := strings.TrimSpace(scanner.Text())
		if symbol == "" || strings.HasPrefix(symbol, "#") {
			continue
		}
		if !strings.Contains(exports, "\n\t"+symbol+"\n") {
			t.Errorf("required compatibility export %q is missing", symbol)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}
