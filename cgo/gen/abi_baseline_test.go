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
	// THE \r STRIP IS LOAD BEARING AND IT IS A FIX. The anchor below is "\n\t" + symbol + "\n",
	// and on a CRLF checkout -- which is what core.autocrlf=true gives every Windows clone of
	// this repo -- the octet after the symbol is \r, so NOT ONE of this file's symbols matched
	// and every assertion here failed. It was invisible because the gen package did not compile
	// on Windows at all: golang.org/x/tools was missing its go.sum h1 line, so `go test ./gen`
	// answered "setup failed" before reaching this.
	exports := "\n" + strings.ReplaceAll(string(defBytes), "\r\n", "\n") + "\n"
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
