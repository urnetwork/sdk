// SPDX-License-Identifier: MPL-2.0
package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Smoke/package/native builds consume committed source. Only an explicit
// generate command may update it, including when an earlier ALL gate failed.
func TestLanguageBuildsCheckBindingsBeforePackaging(t *testing.T) {
	for _, language := range []string{"python", "ruby", "rust", "java", "csharp"} {
		for _, target := range []string{"native", "package", "smoke"} {
			for _, stale := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stale_%t", language, target, stale), func(t *testing.T) {
					fixture := t.TempDir()
					copyFile(path("packaging/language.mk"), filepath.Join(fixture, "packaging/language.mk"))
					copyFile(path(language, "Makefile"), filepath.Join(fixture, language, "Makefile"))
					logPath := filepath.Join(fixture, "commands")
					goTool := filepath.Join(fixture, "fixture-go")
					toolScript(t, goTool, fmt.Sprintf(`
printf '%%s %%s\n' "$5" "${6-}" >> %s
if [ "$5" = check-generated ] && [ %t = true ]; then
  echo 'fixture bindings stale: run make generate explicitly' >&2
  exit 19
fi
`, toolQuote(logPath), stale))
					command := exec.Command("make", "--no-print-directory", target, "GO="+goTool)
					command.Dir = filepath.Join(fixture, language)
					output, err := command.CombinedOutput()
					if (err != nil) != stale {
						t.Fatalf("stale=%t build error=%v: %s", stale, err, output)
					}
					commands := string(read(logPath))
					wanted := "check-generated \n"
					if !stale {
						action := target
						if target == "smoke" {
							action = "package"
						}
						wanted += action + " " + language + "\n"
						if target == "smoke" {
							wanted += "check " + language + "\n"
						}
					}
					if commands != wanted {
						t.Fatalf("build mutated source or bypassed freshness gate: commands=%q, want %q", commands, wanted)
					}
				})
			}
		}
	}
}

func TestLanguageGenerateRemainsExplicit(t *testing.T) {
	fixture := t.TempDir()
	copyFile(path("packaging/language.mk"), filepath.Join(fixture, "packaging/language.mk"))
	copyFile(path("python/Makefile"), filepath.Join(fixture, "python/Makefile"))
	goTool := filepath.Join(fixture, "fixture-go")
	toolScript(t, goTool, "printf '%s\\n' \"$5\"\n")
	command := exec.Command("make", "--no-print-directory", "generate", "GO="+goTool)
	command.Dir = filepath.Join(fixture, "python")
	output, err := command.CombinedOutput()
	if err != nil || !strings.HasSuffix(string(output), "\ngenerate\n") {
		t.Fatalf("explicit generation unavailable: %v: %s", err, output)
	}
	if strings.Contains(string(output), "check-generated") {
		t.Fatal("explicit source update unexpectedly became a read-only check")
	}
}
