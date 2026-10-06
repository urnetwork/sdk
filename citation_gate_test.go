// Every test a production comment in this repository cites is declared here, exactly once.
//
// This corpus discharges an argument in a comment by naming the test that holds it. A name that
// resolves to nothing turns the discharge into decoration: the sentence still reads as though
// something holds it. The gate was urmessage/citationgate_test.go's first half, and it walked the
// whole repository, so it held the core prose too. When messaging moved to
// github.com/urnetwork/message, the half that reads messaging prose went with it and this port
// keeps the core half, with the same walk, net, dispositions and controls.
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

// Cited names that live in another repository, each with where it lives and why this file cannot
// simply look. Held both ways: an entry this repository declares after all is excusing a name the
// plain rule would pass, and an entry no production comment cites is excusing nothing. Empty: no
// core comment cites a test outside this repository.
var citationDeclaredElsewhere = map[string]string{}

// Names the language reserves, cited as a mechanism rather than as a case. A package may declare
// at most one TestMain, so "exactly one declaration in this repository" is the wrong question to
// ask of it. Held both ways, like the table above.
var citationIsNotACase = map[string]string{
	"TestMain": "go's own per-package entry point, named as a mechanism rather than as a case; " +
		"a repository has one per package and the count is meaningless",
}

// Directory names the walk does not enter, with why. The walk counts the .go files each skipped
// directory holds and the gate prints them, so what this narrowing removes is on the page.
var citationSkippedDirectoryNames = map[string]string{
	".git":     "git's own objects",
	"testdata": "fixtures, which are inputs to a test rather than prose",
	"vendor":   "vendored code, which is not this corpus",
	"build":    "build output; this name also covers the gomobile module, as it did before the port",
}

// Every test this repository declares and every citation of one in its production prose, read
// once and answered to by holding.
type citationCorpus struct {
	// test name -> the files declaring it
	declaredPaths map[string][]string
	// test name -> the comment sites citing it
	citedSites map[string][]string
	// .go files walked, and how many of them are not tests
	total, production int
	// skipped directory -> the .go files beneath it
	skippedGoFileCounts map[string]int
}

// Which disposition of the rule holds one cited name, or none of them.
type citationHolding int

const (
	citationHeldHere citationHolding = iota
	citationHeldElsewhere
	citationHeldNotACase
	citationUnheld
)

// The one reading of the rule: a carve-out first, then exactly one declaration here.
func (self *citationCorpus) holding(name string) citationHolding {
	if _, carved := citationIsNotACase[name]; carved {
		return citationHeldNotACase
	}
	if _, carved := citationDeclaredElsewhere[name]; carved {
		return citationHeldElsewhere
	}
	if len(self.declaredPaths[name]) == 1 {
		return citationHeldHere
	}
	return citationUnheld
}

// The spelling of a cited test, stated once for the one rule that reads it.
var citationNet = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*`)

// Walks root, reading every .go file's test declarations and every production file's comments.
// A citation is in a comment and nowhere else: a production file cannot call a test, so reading
// only the comment half keeps the subject the prose, which is what rots.
func citationScan(t *testing.T, root string) *citationCorpus {
	// a test function's declaration, spelling the same names citationNet finds
	declaration := regexp.MustCompile(`(?m)^func\s+(Test[A-Z][A-Za-z0-9_]*)\s*\(`)

	corpus := &citationCorpus{
		declaredPaths:       map[string][]string{},
		citedSites:          map[string][]string{},
		skippedGoFileCounts: map[string]int{},
	}
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
				corpus.skippedGoFileCounts[rel] = countGoFiles(path)
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
		corpus.total += 1
		for _, one := range declaration.FindAllStringSubmatch(string(source), -1) {
			corpus.declaredPaths[one[1]] = append(corpus.declaredPaths[one[1]], rel)
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		corpus.production += 1
		for at, line := range strings.Split(string(source), "\n") {
			cut := strings.Index(line, "//")
			if cut < 0 {
				continue
			}
			for _, name := range citationNet.FindAllString(line[cut:], -1) {
				corpus.citedSites[name] = append(corpus.citedSites[name], fmt.Sprintf("%s:%d", rel, at+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return corpus
}

// The declared name sharing the longest prefix with a dangling one, as a suffix for the failure,
// or "" when nothing is close. A two-word rename then says so in the failure.
func citationNearest(name string, declaredPaths map[string][]string) string {
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

// The walk starts at this repository's root, which is this package's directory; go.mod saying so
// is the control that the walk is not reading a subtree. The controls run over the same maps as
// the property: a name declared nowhere must answer zero, and three tests core prose cites must
// answer exactly one declaration and at least one citation each. A walk that read no files would
// answer zero for all of them.
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
	scan := citationScan(t, root)

	// the controls, in the same query and over the same maps
	const absent = "TestAMemberRemovedByACommitCannotDeriveTheEpochThatCommitOpens"
	if found := len(scan.declaredPaths[absent]); found != 0 {
		t.Fatalf("CONTROL FAILED: %s resolves to %d declaration(s). It is a citation once found "+
			"dangling and no repository declares it; a non-zero answer means the net is matching "+
			"something that is not a declaration", absent, found)
	}
	for _, present := range []string{
		"TestRpcGobExtenderStatsComplete",
		"TestLicenseYmlParses",
		"TestLogVerbosityTakesEffectAtRuntime",
	} {
		if found := len(scan.declaredPaths[present]); found != 1 {
			t.Fatalf("CONTROL FAILED: %s resolves to %d declaration(s), want 1. Without this the zero "+
				"above is satisfied by a walk that read no test files at all", present, found)
		}
		if len(scan.citedSites[present]) == 0 {
			t.Fatalf("CONTROL FAILED: no production comment cites %s, which core prose cites; a walk "+
				"that read no production prose would answer the same", present)
		}
	}
	if scan.production == 0 || scan.total == scan.production {
		t.Fatalf("CONTROL FAILED: the walk saw %d .go files of which %d are production; a run with "+
			"no production files or no test files is measuring nothing", scan.total, scan.production)
	}

	// the dispositions, held both ways
	for name, why := range citationDeclaredElsewhere {
		if here := scan.declaredPaths[name]; 0 < len(here) {
			t.Errorf("%s is carved out as living in another repository (%s) and this repository "+
				"declares it at %s. The entry is excusing a name the plain rule would pass; delete it",
				name, why, strings.Join(here, ", "))
		}
		if len(scan.citedSites[name]) == 0 {
			t.Errorf("%s is carved out as living in another repository (%s) and no production "+
				"comment cites it any more. An entry nothing needs is how a disposition rots; delete it",
				name, why)
		}
	}
	for name, why := range citationIsNotACase {
		if len(scan.citedSites[name]) == 0 {
			t.Errorf("%s is carved out as not-a-case (%s) and no production comment names it any "+
				"more; delete the entry", name, why)
		}
	}

	// the property
	names := []string{}
	for name := range scan.citedSites {
		names = append(names, name)
	}
	sort.Strings(names)
	here, elsewhere, exempt := 0, 0, 0
	for _, name := range names {
		switch scan.holding(name) {
		case citationHeldNotACase:
			exempt += 1
			continue
		case citationHeldElsewhere:
			elsewhere += 1
			continue
		case citationHeldHere:
			here += 1
			continue
		}
		found := scan.declaredPaths[name]
		if len(found) == 0 {
			t.Errorf("%s is cited at %s and nothing in this repository declares it. A sentence that "+
				"names a test which does not exist reads as though something holds it and nothing "+
				"does. Either the test is in another repository (add it to citationDeclaredElsewhere, "+
				"having read what it holds) or the citation is stale and the right name goes here.%s",
				name, strings.Join(scan.citedSites[name], ", "), citationNearest(name, scan.declaredPaths))
			continue
		}
		t.Errorf("%s is cited at %s and this repository declares it %d times (%s), so the citation "+
			"does not say which one", name, strings.Join(scan.citedSites[name], ", "), len(found),
			strings.Join(found, ", "))
	}

	// the complement, printed beside what was asserted
	skipped := []string{}
	for directory, count := range scan.skippedGoFileCounts {
		skipped = append(skipped, fmt.Sprintf("%s (%d .go)", directory, count))
	}
	sort.Strings(skipped)
	t.Logf("%d .go files walked, %d of them production; %d distinct Test... names declared here",
		scan.total, scan.production, len(scan.declaredPaths))
	t.Logf("directories not walked: %v", skipped)
	t.Logf("%d distinct names cited in production prose: %d declared here exactly once, %d carved "+
		"out to another repository, %d carved out as not-a-case", len(names), here, elsewhere, exempt)
	if here == 0 {
		t.Errorf("no cited name resolves inside this repository at all, so the rule above is " +
			"vacuous and only the carve-outs are being exercised")
	}
}
