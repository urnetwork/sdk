// Every test a production comment in this repository cites is declared here, exactly once.
//
// This corpus discharges an argument in a comment by naming the test that holds it. A name that
// resolves to nothing turns the discharge into decoration: the sentence still reads as though
// something holds it. The gate was urmessage/citationgate_test.go's first half, and it walked the
// whole repository, so it held the core prose too. When messaging moved to
// github.com/urnetwork/message, the half that reads messaging prose went with it and this port
// keeps the core half, with the same net and dispositions. Its walk is wider than the original's,
// which did not enter build/, the gomobile module: build/ is production prose like any other.
package sdk

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Names the language reserves, cited as a mechanism rather than as a case. A package may declare
// at most one TestMain, so "exactly one declaration in this repository" is the wrong question to
// ask of it. Held both ways: an entry no production comment cites is excusing nothing.
var citationIsNotACase = map[string]string{
	"TestMain": "go's own per-package entry point, named as a mechanism rather than as a case; " +
		"a repository has one per package and the count is meaningless",
}

// Directory names the walk does not enter, with why. None of them may hold this repository's
// production prose, and the gate holds that rather than trusting it: a skipped directory that is
// not a testdata directory must hold no .go file at all. A testdata directory's .go files are
// fixtures, which the go command builds into no package; the gate prints how many each holds.
var citationSkippedDirectoryNames = map[string]string{
	".git":     "git's own objects",
	"testdata": "fixtures, which are inputs to a test rather than prose",
	"vendor":   "vendored code, which is not this corpus; this repository vendors nothing",
}

// The walk starts at this repository's root, which is this package's directory; go.mod saying so
// is the control that the walk is not reading a subtree. The controls run over the same maps as
// the property: a name declared nowhere must answer zero, this gate's own name must answer exactly
// one declaration, and the prose must cite at least one test. A walk that read no files would
// answer zero for all three.
//
// What must be present is taken from the run, never from a list of test names kept here. The gate
// once pinned three tests that core prose cites, and within a day sdk main renamed one of them
// with its citation (06f33802): the citation was sound and the pin failed. A rename that carries
// its citation along is not this gate's business; a rename that leaves the citation behind is,
// and the property below reports it.
func TestEveryTestNameCitedInThisRepositorysProductionProseResolvesToOneDeclaration(t *testing.T) {
	const root = "."
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	goModText := strings.ReplaceAll(string(goMod), "\r\n", "\n")
	if !strings.HasPrefix(goModText, "module github.com/urnetwork/sdk\n") &&
		!strings.Contains(goModText, "\nmodule github.com/urnetwork/sdk\n") {
		t.Fatalf("CONTROL FAILED: %s/go.mod is not github.com/urnetwork/sdk's, so this walk is not the repository's", root)
	}

	// test name -> the files declaring it
	declaredPaths := map[string][]string{}
	// test name -> the comment sites citing it
	citedSites := map[string][]string{}
	// .go files walked, and how many of them are not tests
	total, production := 0, 0
	// skipped directory -> the .go files beneath it
	skippedGoFileCounts := map[string]int{}

	// Walks root, reading every .go file's test declarations and every production file's comments.
	// A citation is in a comment and nowhere else: a production file cannot call a test, so reading
	// only the comment half keeps the subject the prose, which is what rots.
	scan := func() {
		// a test function's declaration, and a citation of one, spelling the same names
		declaration := regexp.MustCompile(`(?m)^func\s+(Test[A-Z][A-Za-z0-9_]*)\s*\(`)
		citation := regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*`)
		countGoFiles := func(directory string) int {
			count := 0
			filepath.Walk(directory, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr == nil && !info.IsDir() && strings.HasSuffix(path, ".go") {
					count += 1
				}
				return nil
			})
			return count
		}
		err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel := filepath.ToSlash(func() string {
				at, _ := filepath.Rel(root, path)
				return at
			}())
			if info.IsDir() {
				if _, skipped := citationSkippedDirectoryNames[info.Name()]; skipped && path != root {
					skippedGoFileCounts[rel] = countGoFiles(path)
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			source, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			total += 1
			for _, one := range declaration.FindAllStringSubmatch(string(source), -1) {
				declaredPaths[one[1]] = append(declaredPaths[one[1]], rel)
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			production += 1
			for at, line := range strings.Split(string(source), "\n") {
				cut := strings.Index(line, "//")
				if cut < 0 {
					continue
				}
				for _, name := range citation.FindAllString(line[cut:], -1) {
					citedSites[name] = append(citedSites[name], fmt.Sprintf("%s:%d", rel, at+1))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	scan()

	// the controls, in the same query and over the same maps
	const absent = "TestAMemberRemovedByACommitCannotDeriveTheEpochThatCommitOpens"
	if found := len(declaredPaths[absent]); found != 0 {
		t.Fatalf("CONTROL FAILED: %s resolves to %d declaration(s). It is a citation once found "+
			"dangling and no repository declares it; a non-zero answer means the net is matching "+
			"something that is not a declaration", absent, found)
	}
	if found := len(declaredPaths[t.Name()]); found != 1 {
		t.Fatalf("CONTROL FAILED: %s, this gate itself, resolves to %d declaration(s), want 1. Without "+
			"this the zero above is satisfied by a walk that read no test files at all", t.Name(), found)
	}
	if len(citedSites) == 0 {
		t.Fatalf("CONTROL FAILED: no production comment cites any test, across %d production files, "+
			"and core prose does cite tests; a walk that read no production prose would answer the same",
			production)
	}
	if production == 0 || total == production {
		t.Fatalf("CONTROL FAILED: the walk saw %d .go files of which %d are production; a run with "+
			"no production files or no test files is measuring nothing", total, production)
	}

	// the carve-outs, held both ways
	for name, why := range citationIsNotACase {
		if len(citedSites[name]) == 0 {
			t.Errorf("%s is carved out as not-a-case (%s) and no production comment names it any "+
				"more; delete the entry", name, why)
		}
	}

	// The declared name sharing the longest prefix with a dangling one, as a suffix for the
	// failure, or "" when nothing is close. A two-word rename then says so in the failure.
	nearest := func(name string) string {
		best, longest := "", 0
		for candidate := range declaredPaths {
			shared := 0
			for shared < len(name) && shared < len(candidate) && name[shared] == candidate[shared] {
				shared += 1
			}
			if longest < shared {
				best, longest = candidate, shared
			}
		}
		if best == "" || longest < 12 {
			return ""
		}
		return fmt.Sprintf(" The declared name sharing the longest prefix (%d characters) is %s; read "+
			"what it holds before writing it in.", longest, best)
	}

	// the property: a carve-out, or exactly one declaration here
	names := []string{}
	for name := range citedSites {
		names = append(names, name)
	}
	sort.Strings(names)
	here, exempt := 0, 0
	for _, name := range names {
		if _, carved := citationIsNotACase[name]; carved {
			exempt += 1
			continue
		}
		found := declaredPaths[name]
		switch len(found) {
		case 1:
			here += 1
		case 0:
			t.Errorf("%s is cited at %s and nothing in this repository declares it. A sentence that "+
				"names a test which does not exist reads as though something holds it and nothing "+
				"does. Either the citation is stale and the right name goes here, or the test lives in "+
				"another repository, and this gate needs a table of such names, each with where it "+
				"lives and why this file cannot look, held both ways like the carve-outs above.%s",
				name, strings.Join(citedSites[name], ", "), nearest(name))
		default:
			t.Errorf("%s is cited at %s and this repository declares it %d times (%s), so the citation "+
				"does not say which one", name, strings.Join(citedSites[name], ", "), len(found),
				strings.Join(found, ", "))
		}
	}

	// the complement, printed beside what was asserted, and asserted: no skip removes a .go file
	// that is not a testdata fixture
	skipped := []string{}
	for directory, count := range skippedGoFileCounts {
		skipped = append(skipped, fmt.Sprintf("%s (%d .go)", directory, count))
		if filepath.Base(filepath.FromSlash(directory)) != "testdata" && count != 0 {
			t.Errorf("the walk does not enter %s, and it holds %d .go file(s). Only a testdata "+
				"directory's .go files may be skipped, since the go command builds no package from "+
				"them; read what these are, and walk them or give the skip a reason that holds", directory, count)
		}
	}
	sort.Strings(skipped)
	t.Logf("%d .go files walked, %d of them production; %d distinct Test... names declared here",
		total, production, len(declaredPaths))
	t.Logf("directories not walked: %v", skipped)
	t.Logf("%d distinct names cited in production prose: %d declared here exactly once, %d carved "+
		"out as not-a-case", len(names), here, exempt)
	if here == 0 {
		t.Errorf("no cited name resolves inside this repository at all, so the rule above is " +
			"vacuous and only the carve-outs are being exercised")
	}
}
