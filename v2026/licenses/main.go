// Regenerates ../license.yml, the license list the SDK embeds and every app
// shows under Account -> Settings -> Licenses (sdk.GetLicenses).
//
// Run from the sdk repo:
//
//	go run ./licenses                  # regenerate license.yml
//	go run ./licenses -skip android    # regenerate, keeping android's entries as they are
//	go run ./licenses -check sdk       # fail if the SDK's Go modules or extra.yml drifted
//	go -C ../sdk run ./licenses -check android   # from an app repo's build
//	go run ./licenses -check web -mmm-dir /path/to/mmm   # site outside the sibling checkouts
//
// ~/urnetwork is a monoroot: the sdk and the app repos are separate git repos
// checked out side by side. The generator reads each sibling at whatever commit
// is checked out and records that commit under `sources`. A sibling that is not
// checked out keeps its existing entries; one that is present but fails to
// collect is an error (pass -skip to keep its entries deliberately).
// -mmm-dir selects the site's checkout when it lives outside those siblings,
// as it does in the release build. Other sources still come from the SDK's siblings.
//
// Collectors, one per origin:
//
//	go         the SDK's Go modules for each build: gomobile (android, apple),
//	           cgo (windows, linux) and wasm (web, extension), plus the Go
//	           standard library
//	extra      licenses/extra.yml: data attributions, fonts, vendored C/C++
//	maven      android: the play, github and solana_dapp release runtime classpaths, resolved
//	           by gradle; licenses from the published POMs (network)
//	swiftpm    apple: Package.resolved, license files from Xcode's package checkouts
//	npm-web    ur.io: production dependencies of react/ and astro/
//	npm-extension  extension: production dependencies
//
// -check compares only what identifies an entry (origin, name, version, apps),
// never the network-fetched text, so it runs offline.
package main

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var allApps = []string{"android", "apple", "windows", "linux", "web", "extension"}

// each origin, the sibling repo it reads (relative to the monoroot), and the
// apps its entries apply to
type originSpec struct {
	name string
	repo string
}

var origins = []originSpec{
	{"go", "sdk"},
	{"extra", "sdk"},
	{"maven", "android"},
	{"swiftpm", "apple"},
	{"npm-web", "mmm"},
	{"npm-extension", "extension"},
}

// the -check targets: the name an app repo passes, and the origins it owns
var checkTargets = map[string][]string{
	"sdk":       {"go", "extra"},
	"android":   {"maven"},
	"apple":     {"swiftpm"},
	"web":       {"npm-web"},
	"extension": {"npm-extension"},
	// windows and linux ship only the SDK and vendored libraries (extra.yml)
	"windows": {"go", "extra"},
	"linux":   {"go", "extra"},
}

type licenseFile struct {
	Sources []source           `yaml:"sources"`
	Entries []*entry           `yaml:"entries"`
	Texts   map[string]*string `yaml:"texts"`
}

type source struct {
	Origin string `yaml:"origin"`
	Repo   string `yaml:"repo"`
	Commit string `yaml:"commit,omitempty"`
}

type entry struct {
	Name      string   `yaml:"name"`
	Version   string   `yaml:"version,omitempty"`
	Kind      string   `yaml:"kind"`
	Origin    string   `yaml:"origin"`
	Apps      []string `yaml:"apps,flow"`
	Url       string   `yaml:"url,omitempty"`
	Spdx      string   `yaml:"spdx,omitempty"`
	Copyright string   `yaml:"copyright,omitempty"`
	Notice    string   `yaml:"notice,omitempty"`
	Text      string   `yaml:"text"`

	// the text itself while collecting; Text becomes its id on write
	text string
}

func (self *entry) key() string {
	return self.Origin + "\x00" + self.Name + "\x00" + self.Version
}

type extraFile struct {
	Entries       []extraEntry `yaml:"entries"`
	SkipGoModules []string     `yaml:"skip_go_modules"`
	NpmBuildOnly  []string     `yaml:"npm_build_only"`
	NpmShallow    []string     `yaml:"npm_shallow"`
}

type extraEntry struct {
	Name      string   `yaml:"name"`
	Version   string   `yaml:"version"`
	Kind      string   `yaml:"kind"`
	Origin    string   `yaml:"origin"`
	Apps      []string `yaml:"apps"`
	Url       string   `yaml:"url"`
	Spdx      string   `yaml:"spdx"`
	Copyright string   `yaml:"copyright"`
	Notice    string   `yaml:"notice"`
	TextFile  string   `yaml:"text_file"`
	Text      string   `yaml:"text"`
}

var (
	sdkDir  string
	rootDir string
	mmmDir  string
	extra   extraFile
	offline bool
)

func main() {
	check := flag.String("check", "", "compare license.yml against this target instead of writing it: "+strings.Join(slices.Sorted(maps.Keys(checkTargets)), ", "))
	skip := flag.String("skip", "", "comma separated origins to keep as they are in license.yml")
	mmmCheckout := flag.String("mmm-dir", "", "mmm checkout containing ur.io (default: sibling of the SDK)")
	flag.Parse()

	if err := run(*check, *skip, *mmmCheckout); err != nil {
		fmt.Fprintf(os.Stderr, "licenses: %v\n", err)
		os.Exit(1)
	}
}

func run(check string, skip string, mmmCheckout string) error {
	var err error
	sdkDir, err = findSdkDir()
	if err != nil {
		return err
	}
	rootDir = filepath.Dir(sdkDir)
	mmmDir = filepath.Join(rootDir, "mmm")
	if mmmCheckout != "" {
		mmmDir, err = filepath.Abs(mmmCheckout)
		if err != nil {
			return fmt.Errorf("mmm checkout: %w", err)
		}
	}

	extraBytes, err := os.ReadFile(filepath.Join(sdkDir, "licenses", "extra.yml"))
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(extraBytes, &extra); err != nil {
		return fmt.Errorf("extra.yml: %w", err)
	}

	existing, err := readLicenseFile(filepath.Join(sdkDir, "license.yml"))
	if err != nil {
		return err
	}

	if check != "" {
		targets, ok := checkTargets[check]
		if !ok {
			return fmt.Errorf("unknown -check target %q", check)
		}
		offline = true
		if err := runCheck(existing, targets); err != nil {
			return err
		}
		var targetEntries []*entry
		for _, e := range existing.Entries {
			if slices.Contains(targets, e.Origin) {
				targetEntries = append(targetEntries, e)
			}
		}
		return checkPolicy(targetEntries, func(e *entry) string {
			if t := existing.Texts[e.Text]; t != nil {
				return *t
			}
			return ""
		})
	}

	skipped := map[string]bool{}
	for _, origin := range strings.Split(skip, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			skipped[origin] = true
		}
	}

	out := &licenseFile{}
	for _, spec := range origins {
		repoDir := filepath.Join(rootDir, spec.repo)
		if spec.repo == "mmm" {
			repoDir = mmmDir
		}
		keep := skipped[spec.name]
		if !keep && !exists(repoDir) {
			fmt.Fprintf(os.Stderr, "licenses: %s: %s is not checked out, keeping its entries\n", spec.name, spec.repo)
			keep = true
		}
		if keep {
			out.keepOrigin(existing, spec.name)
			continue
		}
		fmt.Fprintf(os.Stderr, "licenses: collecting %s\n", spec.name)
		entries, err := collect(spec.name)
		if err != nil {
			return fmt.Errorf("%s: %w", spec.name, err)
		}
		s := source{Origin: spec.name, Repo: spec.repo}
		// the sdk's own commit would change with every commit of license.yml
		if spec.repo != "sdk" {
			s.Commit = gitCommit(repoDir)
		}
		out.Sources = append(out.Sources, s)
		out.Entries = append(out.Entries, entries...)
	}
	if err := checkPolicy(out.Entries, func(e *entry) string { return e.text }); err != nil {
		return err
	}
	return out.write(filepath.Join(sdkDir, "license.yml"))
}

// runCheck recollects the target's origins offline and reports entries that
// are missing from, or stale in, license.yml
func runCheck(existing *licenseFile, targets []string) error {
	var problems []string
	for _, origin := range targets {
		entries, err := collect(origin)
		if err != nil {
			return fmt.Errorf("%s: %w", origin, err)
		}
		want := map[string]*entry{}
		for _, e := range entries {
			want[e.key()] = e
		}
		have := map[string]*entry{}
		for _, e := range existing.Entries {
			if e.Origin == origin {
				have[e.key()] = e
			}
		}
		for k, e := range want {
			h, ok := have[k]
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("missing %s %s %s", origin, e.Name, e.Version))
			case !slices.Equal(h.Apps, e.Apps):
				problems = append(problems, fmt.Sprintf("%s %s %s: apps %v, want %v", origin, e.Name, e.Version, h.Apps, e.Apps))
			case origin == "extra" && !extraEqual(h, e, existing):
				problems = append(problems, fmt.Sprintf("extra %s: changed in extra.yml", e.Name))
			}
		}
		for k, e := range have {
			if _, ok := want[k]; !ok {
				problems = append(problems, fmt.Sprintf("stale %s %s %s", origin, e.Name, e.Version))
			}
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("license.yml is out of date (run `go run ./licenses` in the sdk repo and commit license.yml):\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

func extraEqual(have *entry, want *entry, existing *licenseFile) bool {
	text := ""
	if t := existing.Texts[have.Text]; t != nil {
		text = *t
	}
	return have.Kind == want.Kind &&
		have.Url == want.Url &&
		have.Spdx == want.Spdx &&
		have.Copyright == want.Copyright &&
		have.Notice == want.Notice &&
		text == normalizeText(want.text)
}

func collect(origin string) ([]*entry, error) {
	switch origin {
	case "go":
		return collectGo()
	case "extra":
		return collectExtra()
	case "maven":
		return collectMaven()
	case "swiftpm":
		return collectSwiftpm()
	case "npm-web":
		return collectNpm("npm-web", []string{"web"}, filepath.Join(mmmDir, "ur.io/react"), filepath.Join(mmmDir, "ur.io/astro"))
	case "npm-extension":
		return collectNpm("npm-extension", []string{"extension"}, "extension")
	}
	return nil, fmt.Errorf("unknown origin")
}

// Go

type goTarget struct {
	dir      string // relative to the sdk dir
	env      []string
	tags     string
	packages []string
	apps     []string
}

var goTargets = []goTarget{
	{".", []string{"GOOS=android", "GOARCH=arm64", "CGO_ENABLED=1"}, "sdk_mobile_bind", []string{".", "golang.org/x/mobile/bind/java", "golang.org/x/mobile/bind/seq"}, []string{"android"}},
	{".", []string{"GOOS=ios", "GOARCH=arm64", "CGO_ENABLED=1"}, "sdk_mobile_bind", []string{".", "golang.org/x/mobile/bind/objc", "golang.org/x/mobile/bind/seq"}, []string{"apple"}},
	{".", []string{"GOOS=ios", "GOARCH=arm64", "CGO_ENABLED=1"}, "sdk_mobile_bind,ios_extension", []string{".", "golang.org/x/mobile/bind/objc", "golang.org/x/mobile/bind/seq"}, []string{"apple"}},
	{".", []string{"GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=1"}, "sdk_mobile_bind", []string{".", "golang.org/x/mobile/bind/objc", "golang.org/x/mobile/bind/seq"}, []string{"apple"}},
	{"cgo", []string{"GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=1"}, "", []string{"."}, []string{"windows"}},
	{"cgo", []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=1"}, "", []string{"."}, []string{"linux"}},
	{"js", []string{"GOOS=js", "GOARCH=wasm"}, "", []string{"."}, []string{"web", "extension"}},
}

type goPackage struct {
	Standard bool
	Module   *struct {
		Path    string
		Version string
		Dir     string
		Replace *struct {
			Path    string
			Version string
			Dir     string
		}
	}
}

func collectGo() ([]*entry, error) {
	type module struct {
		path, version, dir string
		apps               map[string]bool
	}
	modules := map[string]*module{}
	stdApps := map[string]bool{}

	for _, target := range goTargets {
		args := []string{"list", "-deps", "-json=Standard,Module"}
		if target.tags != "" {
			args = append(args, "-tags", target.tags)
		}
		args = append(args, target.packages...)
		cmd := exec.Command("go", args...)
		cmd.Dir = filepath.Join(sdkDir, target.dir)
		cmd.Env = append(os.Environ(), target.env...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list in %s (%v): %w\n%s", target.dir, target.env, err, stderr.String())
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		for {
			var pkg goPackage
			if err := dec.Decode(&pkg); err == io.EOF {
				break
			} else if err != nil {
				return nil, err
			}
			if pkg.Standard {
				for _, app := range target.apps {
					stdApps[app] = true
				}
				continue
			}
			if pkg.Module == nil {
				continue
			}
			path, version, dir := pkg.Module.Path, pkg.Module.Version, pkg.Module.Dir
			if pkg.Module.Replace != nil {
				dir = pkg.Module.Replace.Dir
				if pkg.Module.Replace.Version != "" {
					version = pkg.Module.Replace.Version
				}
			}
			if slices.Contains(extra.SkipGoModules, path) {
				continue
			}
			// the sdk's own build modules (cgo, js) are the sdk
			if strings.HasPrefix(path, "github.com/urnetwork/sdk/v2026/") {
				continue
			}
			m := modules[path]
			if m == nil {
				m = &module{path: path, version: version, dir: dir, apps: map[string]bool{}}
				modules[path] = m
			}
			for _, app := range target.apps {
				m.apps[app] = true
			}
		}
	}

	var entries []*entry
	for _, m := range modules {
		text, err := readLicenseFiles(m.dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.path, err)
		}
		if text == "" {
			return nil, fmt.Errorf("%s (%s) has no license file; add it to skip_go_modules in extra.yml with an entry that credits it", m.path, m.dir)
		}
		version := m.version
		if version == "" || version == "v0.0.0" {
			version = ""
		}
		entries = append(entries, &entry{
			Name:      m.path,
			Version:   version,
			Kind:      "software",
			Origin:    "go",
			Apps:      sortedApps(m.apps),
			Url:       goModuleUrl(m.path),
			Spdx:      detectSpdx(text),
			Copyright: extractCopyright(text),
			text:      text,
		})
	}

	// the Go standard library and runtime, compiled into every build
	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return nil, err
	}
	text, err := readLicenseFiles(strings.TrimSpace(string(goroot)))
	if err != nil || text == "" {
		return nil, fmt.Errorf("go standard library license: %v", err)
	}
	entries = append(entries, &entry{
		// no version: the text does not change with the toolchain, and a
		// version would make -check fail on every toolchain bump
		Name:      "Go standard library",
		Kind:      "software",
		Origin:    "go",
		Apps:      sortedApps(stdApps),
		Url:       "https://go.dev",
		Spdx:      detectSpdx(text),
		Copyright: extractCopyright(text),
		text:      text,
	})
	return entries, nil
}

func goModuleUrl(path string) string {
	switch {
	case strings.HasPrefix(path, "github.com/"), strings.HasPrefix(path, "gitlab.com/"):
		parts := strings.Split(path, "/")
		if len(parts) >= 3 {
			return "https://" + strings.Join(parts[:3], "/")
		}
	case strings.HasPrefix(path, "golang.org/x/"):
		return "https://pkg.go.dev/" + path
	}
	return "https://pkg.go.dev/" + path
}

// extra.yml

func collectExtra() ([]*entry, error) {
	var entries []*entry
	for i := range extra.Entries {
		x := &extra.Entries[i]
		e := entry{
			Name:      x.Name,
			Version:   x.Version,
			Kind:      x.Kind,
			Origin:    x.Origin,
			Apps:      x.Apps,
			Url:       x.Url,
			Spdx:      x.Spdx,
			Copyright: x.Copyright,
			Notice:    x.Notice,
		}
		switch {
		case x.TextFile != "":
			b, err := os.ReadFile(filepath.Join(sdkDir, "licenses", x.TextFile))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Name, err)
			}
			e.text = stripTemplateLines(string(b))
		case x.Text != "":
			e.text = x.Text
		default:
			return nil, fmt.Errorf("%s: needs text_file or text", e.Name)
		}
		if slices.Equal(e.Apps, []string{"all"}) {
			e.Apps = slices.Clone(allApps)
		}
		for _, app := range e.Apps {
			if !slices.Contains(allApps, app) {
				return nil, fmt.Errorf("%s: unknown app %q", e.Name, app)
			}
		}
		e.Apps = sortedApps(setOf(e.Apps))
		if e.Origin == "" || e.Kind == "" {
			return nil, fmt.Errorf("%s: needs kind and origin", e.Name)
		}
		// extra entries are keyed by origin "extra" so -check can own them
		e.Origin = "extra"
		entries = append(entries, &e)
	}
	return entries, nil
}

// Maven (android)

// every flavor the release pipeline ships (build/all/run.sh assembles play,
// solana_dapp, and github)
var gradleConfigurations = []string{"playReleaseRuntimeClasspath", "githubReleaseRuntimeClasspath", "solana_dappReleaseRuntimeClasspath"}

var gradleDepRe = regexp.MustCompile(`[+\\]--- ([^:\s]+):([^:\s]+)(?::([^\s]+))?(?: -> ([^\s]+))?`)

func collectMaven() ([]*entry, error) {
	projectDir := filepath.Join(rootDir, "android", "app")
	coords := map[string]bool{}
	for _, configuration := range gradleConfigurations {
		cmd := exec.Command("./gradlew", "-q", ":app:dependencies", "--configuration", configuration)
		cmd.Dir = projectDir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("gradle %s: %w\n%s", configuration, err, stderr.String())
		}
		for _, line := range strings.Split(string(out), "\n") {
			// constraints are not dependencies
			if strings.HasSuffix(line, "(c)") || strings.Contains(line, "--- project ") {
				continue
			}
			m := gradleDepRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			version := m[3]
			if m[4] != "" {
				version = m[4]
			}
			// "1.2 (*)" and friends
			version = strings.Fields(version + " ")[0]
			if version == "" {
				continue
			}
			coords[m[1]+":"+m[2]+":"+version] = true
		}
	}
	if len(coords) == 0 {
		return nil, errors.New("gradle reported no runtime dependencies")
	}

	var entries []*entry
	for _, coord := range slices.Sorted(maps.Keys(coords)) {
		parts := strings.SplitN(coord, ":", 3)
		e := &entry{
			Name:    parts[0] + ":" + parts[1],
			Version: parts[2],
			Kind:    "software",
			Origin:  "maven",
			Apps:    []string{"android"},
		}
		if !offline {
			info, err := mavenLicense(parts[0], parts[1], parts[2])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", coord, err)
			}
			e.Url = info.url
			e.Spdx = info.spdx
			e.Copyright = info.copyright
			e.text = info.text
		}
		entries = append(entries, e)
	}
	return entries, nil
}

type pom struct {
	Url    string `xml:"url"`
	Parent *struct {
		GroupId    string `xml:"groupId"`
		ArtifactId string `xml:"artifactId"`
		Version    string `xml:"version"`
	} `xml:"parent"`
	Organization struct {
		Name string `xml:"name"`
	} `xml:"organization"`
	Licenses []struct {
		Name string `xml:"name"`
		Url  string `xml:"url"`
	} `xml:"licenses>license"`
	Scm struct {
		Url string `xml:"url"`
	} `xml:"scm"`
}

var mavenRepos = []string{
	"https://dl.google.com/android/maven2",
	"https://repo1.maven.org/maven2",
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func fetchPom(group, artifact, version string) (*pom, error) {
	path := fmt.Sprintf("%s/%s/%s/%s-%s.pom", strings.ReplaceAll(group, ".", "/"), artifact, version, artifact, version)
	body, err := fetchPomBytes(path)
	if err != nil {
		return nil, err
	}
	var p pom
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.CharsetReader = latin1Reader
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

// fetchPomBytes reads a released pom, which never changes, through a cache in
// the user cache dir
func fetchPomBytes(path string) ([]byte, error) {
	cacheDir, _ := os.UserCacheDir()
	cachePath := filepath.Join(cacheDir, "urnetwork-licenses", "maven", path)
	if b, err := os.ReadFile(cachePath); err == nil {
		return b, nil
	}
	var lastErr error
	for attempt := 0; attempt < 6; attempt += 1 {
		if attempt > 0 {
			// Maven Central answers bursts with 429
			time.Sleep(time.Duration(5<<attempt) * time.Second)
		}
		for _, repo := range mavenRepos {
			req, err := http.NewRequest("GET", repo+"/"+path, nil)
			if err != nil {
				return nil, err
			}
			// Maven Central throttles Go's default user agent
			req.Header.Set("User-Agent", "urnetwork-sdk-licenses/1 (+https://github.com/urnetwork/sdk)")
			resp, err := httpClient.Do(req)
			if err != nil {
				lastErr = err
				continue
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				lastErr = err
				continue
			}
			switch resp.StatusCode {
			case http.StatusOK:
				if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err == nil {
					os.WriteFile(cachePath, body, 0644)
				}
				return body, nil
			case http.StatusNotFound:
				lastErr = fmt.Errorf("no pom at %s", path)
			default:
				lastErr = fmt.Errorf("%s/%s: %s", repo, path, resp.Status)
			}
		}
	}
	return nil, lastErr
}

// old poms declare ISO-8859-1
func latin1Reader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(charset) {
	case "iso-8859-1", "latin1", "latin-1":
	default:
		return nil, fmt.Errorf("unsupported pom charset %s", charset)
	}
	b, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return strings.NewReader(string(runes)), nil
}

type mavenLicenseInfo struct {
	url, spdx, copyright, text string
}

func mavenLicense(group, artifact, version string) (*mavenLicenseInfo, error) {
	info := &mavenLicenseInfo{}
	// licenses and urls are inherited from parent poms
	for depth := 0; depth < 6; depth += 1 {
		p, err := fetchPom(group, artifact, version)
		if err != nil {
			return nil, err
		}
		if info.url == "" {
			info.url = cmp.Or(p.Url, p.Scm.Url)
		}
		if info.copyright == "" && p.Organization.Name != "" {
			info.copyright = "Copyright (c) " + p.Organization.Name
		}
		if len(p.Licenses) > 0 {
			var ids []string
			var texts []string
			for _, l := range p.Licenses {
				id := spdxFromName(l.Name, l.Url)
				if id == "" {
					// vendor terms (Play Core, Play services): no text to
					// reproduce, so point at them. GPL-family names would
					// land here too, which is why this is loud.
					if strings.Contains(strings.ToLower(l.Name), "gpl") || strings.Contains(strings.ToLower(l.Name), "general public") {
						return nil, fmt.Errorf("copyleft license %q (%s): review before shipping, then add it to spdxFromName", l.Name, l.Url)
					}
					fmt.Fprintf(os.Stderr, "licenses: %s:%s:%s: vendor license %q, linking to %s\n", group, artifact, version, l.Name, l.Url)
					ids = append(ids, "LicenseRef-"+strings.Trim(nonWordRe.ReplaceAllString(l.Name, "-"), "-"))
					texts = append(texts, fmt.Sprintf("Licensed under the %s:\n%s\n", l.Name, l.Url))
					continue
				}
				ids = append(ids, id)
				text, err := canonicalText(id)
				if err != nil {
					return nil, err
				}
				texts = append(texts, text)
			}
			info.spdx = strings.Join(ids, " OR ")
			info.text = strings.Join(texts, "\n\n----------------------------------------\n\n")
			return info, nil
		}
		if p.Parent == nil {
			break
		}
		group, artifact, version = p.Parent.GroupId, p.Parent.ArtifactId, p.Parent.Version
	}
	return nil, errors.New("no license in the pom or its parents")
}

var (
	nonWordRe = regexp.MustCompile(`[^A-Za-z0-9.]+`)
	mitRe     = regexp.MustCompile(`\bmit\b`)
	bsdRe     = regexp.MustCompile(`\bbsd\b`)
)

func spdxFromName(name string, url string) string {
	s := strings.ToLower(name + " " + url)
	switch {
	case strings.Contains(s, "apache"):
		return "Apache-2.0"
	case mitRe.MatchString(s):
		return "MIT"
	case bsdRe.MatchString(s) && (strings.Contains(s, "3") || strings.Contains(s, "new") || strings.Contains(s, "revised")):
		return "BSD-3-Clause"
	case bsdRe.MatchString(s):
		return "BSD-2-Clause"
	case strings.Contains(s, "mozilla public license"), strings.Contains(s, "mpl"):
		return "MPL-2.0"
	case strings.Contains(s, "eclipse public license") && strings.Contains(s, "1.0"), strings.Contains(s, "epl-v10"):
		return "EPL-1.0"
	case strings.Contains(s, "eclipse public license") && strings.Contains(s, "2.0"):
		return "EPL-2.0"
	case strings.Contains(s, "android software development kit license"), strings.Contains(s, "developer.android.com/studio/terms"):
		return "LicenseRef-Android-SDK"
	}
	return ""
}

func canonicalText(spdx string) (string, error) {
	b, err := os.ReadFile(filepath.Join(sdkDir, "licenses", "texts", spdx+".txt"))
	if err != nil {
		return "", fmt.Errorf("no canonical text for %s: add licenses/texts/%s.txt", spdx, spdx)
	}
	return stripTemplateLines(string(b)), nil
}

// Swift packages (apple)

func collectSwiftpm() ([]*entry, error) {
	resolvedPath := filepath.Join(rootDir, "apple", "app", "app.xcodeproj", "project.xcworkspace", "xcshareddata", "swiftpm", "Package.resolved")
	b, err := os.ReadFile(resolvedPath)
	if err != nil {
		return nil, err
	}
	var resolved struct {
		Pins []struct {
			Identity string `json:"identity"`
			Location string `json:"location"`
			State    struct {
				Revision string `json:"revision"`
				Version  string `json:"version"`
			} `json:"state"`
		} `json:"pins"`
	}
	if err := json.Unmarshal(b, &resolved); err != nil {
		return nil, err
	}

	var entries []*entry
	for _, pin := range resolved.Pins {
		e := &entry{
			Name:    pin.Identity,
			Version: cmp.Or(pin.State.Version, shortRevision(pin.State.Revision)),
			Kind:    "software",
			Origin:  "swiftpm",
			Apps:    []string{"apple"},
			Url:     strings.TrimSuffix(pin.Location, ".git"),
		}
		if !offline {
			dir, err := swiftpmCheckout(pin.Location, pin.State.Revision)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", pin.Identity, err)
			}
			text, err := readLicenseFiles(dir)
			if err != nil {
				return nil, err
			}
			if text == "" {
				return nil, fmt.Errorf("%s: no license file in %s", pin.Identity, dir)
			}
			e.Spdx = detectSpdx(text)
			e.Copyright = extractCopyright(text)
			e.text = text
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

// swiftpmCheckout finds Xcode's checkout of the package at the pinned revision
func swiftpmCheckout(location string, revision string) (string, error) {
	home, _ := os.UserHomeDir()
	name := strings.TrimSuffix(filepath.Base(location), ".git")
	patterns := []string{
		filepath.Join(home, "Library", "Developer", "Xcode", "DerivedData", "*", "SourcePackages", "checkouts", name),
		filepath.Join(rootDir, "apple", "build", "*", "SourcePackages", "checkouts", name),
	}
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, dir := range matches {
			out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
			if err == nil && strings.TrimSpace(string(out)) == revision {
				return dir, nil
			}
		}
	}
	return "", fmt.Errorf("no checkout of %s at %s; resolve packages in Xcode (File > Packages > Resolve Package Versions) and rerun", name, revision)
}

// npm

type packageLockPackage struct {
	Version              string            `json:"version"`
	License              any               `json:"license"`
	Link                 bool              `json:"link"`
	Optional             bool              `json:"optional"`
	Os                   []string          `json:"os"`
	Cpu                  []string          `json:"cpu"`
	Dependencies         map[string]string `json:"dependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

type packageLock struct {
	Packages map[string]packageLockPackage `json:"packages"`
}

// collectNpm walks the lockfile's dependency graph from the project's
// production dependencies, minus the build-only roots in extra.yml. Packages
// pinned to an os or cpu (native build binaries) are skipped: they never reach
// a bundle, and which one is installed depends on the machine.
func collectNpm(origin string, apps []string, projects ...string) ([]*entry, error) {
	byKey := map[string]*entry{}
	for _, project := range projects {
		projectDir := project
		if !filepath.IsAbs(projectDir) {
			projectDir = filepath.Join(rootDir, projectDir)
		}
		b, err := os.ReadFile(filepath.Join(projectDir, "package-lock.json"))
		if err != nil {
			return nil, err
		}
		var lock packageLock
		if err := json.Unmarshal(b, &lock); err != nil {
			return nil, fmt.Errorf("%s/package-lock.json: %w", project, err)
		}
		root, ok := lock.Packages[""]
		if !ok {
			return nil, fmt.Errorf("%s/package-lock.json: no root package", project)
		}

		// resolve a dependency the way node does: the nearest node_modules
		// walking up from the requiring package
		resolve := func(from string, name string) string {
			dir := from
			for {
				candidate := "node_modules/" + name
				if dir != "" {
					candidate = dir + "/node_modules/" + name
				}
				if _, ok := lock.Packages[candidate]; ok {
					return candidate
				}
				if dir == "" {
					return ""
				}
				i := strings.LastIndex(dir, "/node_modules/")
				if i < 0 {
					dir = ""
				} else {
					dir = dir[:i]
				}
			}
		}

		visited := map[string]bool{}
		var visit func(path string, deep bool) error
		visit = func(path string, deep bool) error {
			if visited[path] {
				return nil
			}
			visited[path] = true
			pkg := lock.Packages[path]
			if len(pkg.Os) > 0 || len(pkg.Cpu) > 0 {
				return nil
			}
			name := path[strings.LastIndex(path, "node_modules/")+len("node_modules/"):]
			if pkg.Link || strings.HasPrefix(name, "@urnetwork/") {
				return nil
			}
			e := &entry{
				Name:    name,
				Version: pkg.Version,
				Kind:    "software",
				Origin:  origin,
				Apps:    apps,
				Url:     "https://www.npmjs.com/package/" + name,
				Spdx:    npmLicenseField(pkg.License),
			}
			if byKey[e.key()] == nil {
				if !offline {
					if err := npmLicense(e, filepath.Join(projectDir, path)); err != nil {
						return fmt.Errorf("%s: %w", project, err)
					}
				}
				byKey[e.key()] = e
			}
			if !deep {
				return nil
			}
			for dep := range pkg.Dependencies {
				if p := resolve(path, dep); p != "" {
					if err := visit(p, true); err != nil {
						return err
					}
				} else {
					return fmt.Errorf("%s: %s requires %s, which the lockfile does not have", project, name, dep)
				}
			}
			for dep := range pkg.OptionalDependencies {
				if p := resolve(path, dep); p != "" {
					if err := visit(p, true); err != nil {
						return err
					}
				}
			}
			return nil
		}

		for dep := range root.Dependencies {
			if slices.Contains(extra.NpmBuildOnly, dep) {
				continue
			}
			p := resolve("", dep)
			if p == "" {
				return nil, fmt.Errorf("%s: dependency %s is not in package-lock.json", project, dep)
			}
			if err := visit(p, !slices.Contains(extra.NpmShallow, dep)); err != nil {
				return nil, err
			}
		}
	}
	entries := make([]*entry, 0, len(byKey))
	for _, e := range byKey {
		entries = append(entries, e)
	}
	return entries, nil
}

func npmLicense(e *entry, dir string) error {
	if !exists(dir) {
		return fmt.Errorf("%s is not installed; run npm ci", dir)
	}
	text, err := readLicenseFiles(dir)
	if err != nil {
		return err
	}
	if text == "" {
		if e.Spdx == "" {
			return fmt.Errorf("%s %s has no license file and no license field", e.Name, e.Version)
		}
		text, err = canonicalText(e.Spdx)
		if err != nil {
			return fmt.Errorf("%s %s: %w", e.Name, e.Version, err)
		}
	}
	if e.Spdx == "" || strings.HasPrefix(strings.ToUpper(e.Spdx), "SEE LICENSE") {
		e.Spdx = detectSpdx(text)
	}
	e.Copyright = extractCopyright(text)
	e.text = text
	if repo := npmRepositoryUrl(dir); repo != "" {
		e.Url = repo
	}
	return nil
}

func npmLicenseField(v any) string {
	switch l := v.(type) {
	case string:
		return l
	case map[string]any:
		if t, ok := l["type"].(string); ok {
			return t
		}
	}
	return ""
}

func npmRepositoryUrl(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var p struct {
		Homepage   string `json:"homepage"`
		Repository any    `json:"repository"`
	}
	if json.Unmarshal(b, &p) != nil {
		return ""
	}
	url := ""
	switch r := p.Repository.(type) {
	case string:
		url = r
	case map[string]any:
		url, _ = r["url"].(string)
	}
	url = strings.TrimPrefix(url, "git+")
	url = strings.TrimSuffix(url, ".git")
	url = strings.Replace(url, "git://", "https://", 1)
	url = strings.Replace(url, "ssh://git@", "https://", 1)
	if strings.HasPrefix(url, "github:") {
		url = "https://github.com/" + strings.TrimPrefix(url, "github:")
	} else if !strings.Contains(url, "://") && strings.Count(url, "/") == 1 {
		url = "https://github.com/" + url
	}
	if strings.HasPrefix(url, "https://") {
		return url
	}
	if strings.HasPrefix(p.Homepage, "https://") {
		return p.Homepage
	}
	return ""
}

// license text

var licenseFileRe = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|unlicense)([-._].*)?$`)

// readLicenseFiles reads the license and notice files at the top of dir, in
// name order, joined. Apache NOTICE files must be reproduced with the license.
func readLicenseFiles(dir string) (string, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var texts []string
	for _, de := range des {
		if de.IsDir() || !licenseFileRe.MatchString(de.Name()) {
			continue
		}
		switch strings.ToLower(filepath.Ext(de.Name())) {
		case ".yml", ".yaml", ".json", ".go", ".js", ".mjs", ".ts", ".html", ".xml", ".toml", ".sh", ".py":
			// code and data that happen to be named license*, e.g. the
			// sdk's own license.yml and license.go
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			return "", err
		}
		if t := strings.TrimSpace(string(b)); t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n\n----------------------------------------\n\n"), nil
}

func detectSpdx(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	// whole-text forms that name their own expression
	switch {
	case strings.Contains(t, "Licensed under the `Permissive License Stack`, meaning either of"):
		// Protocol Labs (multiformats, ipfs): Apache-2.0 or MIT
		return "Apache-2.0 OR MIT"
	case strings.Contains(t, "WALLETCONNECT COMMUNITY LICENSE AGREEMENT"):
		return "LicenseRef-WalletConnect-Community-License"
	}
	var ids []string
	has := func(s ...string) bool {
		for _, x := range s {
			if !strings.Contains(t, x) {
				return false
			}
		}
		return true
	}
	if has("Apache License", "Version 2.0") {
		ids = append(ids, "Apache-2.0")
	}
	if has("Permission is hereby granted, free of charge") {
		ids = append(ids, "MIT")
	}
	if has("Redistribution and use in source and binary forms") {
		if strings.Contains(t, "Neither the name") || strings.Contains(t, "names of its contributors") {
			ids = append(ids, "BSD-3-Clause")
		} else {
			ids = append(ids, "BSD-2-Clause")
		}
	}
	if has("Permission to use, copy, modify, and/or distribute this software") || has("Permission to use, copy, modify, and distribute this software for any purpose with or without fee") {
		ids = append(ids, "ISC")
	}
	if has("Mozilla Public License") && (strings.Contains(t, "2.0")) {
		ids = append(ids, "MPL-2.0")
	}
	if has("This is free and unencumbered software released into the public domain") {
		ids = append(ids, "Unlicense")
	}
	if has("CC0 1.0 Universal") {
		ids = append(ids, "CC0-1.0")
	}
	if has("This software is provided 'as-is', without any express or implied warranty") {
		ids = append(ids, "Zlib")
	}
	if has("Blue Oak Model License") {
		ids = append(ids, "BlueOak-1.0.0")
	}
	if has("SIL OPEN FONT LICENSE") || has("SIL Open Font License") {
		ids = append(ids, "OFL-1.1")
	}
	if has("GNU LESSER GENERAL PUBLIC LICENSE") {
		ids = append(ids, "LGPL")
	}
	return strings.Join(ids, " AND ")
}

var copyrightRe = regexp.MustCompile(`(?i)^\s*(copyright\s*(\(c\)|©|\d{4})|©).*`)

func extractCopyright(text string) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !copyrightRe.MatchString(line) || strings.Contains(line, "<") && strings.Contains(line, ">") && !strings.Contains(line, "@") {
			continue
		}
		if !slices.Contains(lines, line) {
			lines = append(lines, line)
		}
		if len(lines) == 5 {
			break
		}
	}
	return strings.Join(lines, "\n")
}

// stripTemplateLines drops the "Copyright (c) <year> <copyright holders>"
// placeholders of the canonical SPDX texts; the entry's copyright field
// carries the real holder
func stripTemplateLines(text string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		l := strings.ToLower(line)
		if strings.Contains(l, "copyright") && (strings.Contains(l, "<year>") || strings.Contains(l, "<copyright holder") || strings.Contains(l, "<dates>") || strings.Contains(l, "<owner>")) {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func normalizeText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n") + "\n"
}

func textId(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:12]
}

// license.yml

func readLicenseFile(path string) (*licenseFile, error) {
	f := &licenseFile{Texts: map[string]*string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	} else if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Texts == nil {
		f.Texts = map[string]*string{}
	}
	return f, nil
}

func (self *licenseFile) keepOrigin(existing *licenseFile, origin string) {
	for _, s := range existing.Sources {
		if s.Origin == origin {
			self.Sources = append(self.Sources, s)
		}
	}
	for _, e := range existing.Entries {
		if e.Origin == origin {
			if t := existing.Texts[e.Text]; t != nil {
				e.text = *t
			}
			self.Entries = append(self.Entries, e)
		}
	}
}

var kindOrder = map[string]int{"data": 0, "software": 1, "font": 2}

func (self *licenseFile) write(path string) error {
	self.Texts = map[string]*string{}
	for _, e := range self.Entries {
		text := normalizeText(e.text)
		id := textId(text)
		self.Texts[id] = &text
		e.Text = id
	}
	slices.SortStableFunc(self.Entries, func(a, b *entry) int {
		if c := cmp.Compare(kindOrder[a.Kind], kindOrder[b.Kind]); c != 0 || a.Kind == "data" {
			// data attributions keep their extra.yml order, which leads
			// with the notices the licenses require be shown
			return c
		}
		return cmp.Or(
			cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)),
			cmp.Compare(a.Version, b.Version),
			cmp.Compare(a.Origin, b.Origin),
		)
	})

	var buf bytes.Buffer
	buf.WriteString("# Code generated by `go run ./licenses` in the sdk repo. DO NOT EDIT.\n")
	buf.WriteString("# Hand-kept entries live in licenses/extra.yml. Embedded by license.go.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	// texts is written last and in id order (yaml.v3 sorts map keys)
	if err := enc.Encode(self); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "licenses: wrote %s: %d entries, %d texts, %d bytes\n", path, len(self.Entries), len(self.Texts), buf.Len())
	return nil
}

// helpers

func findSdkDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && bytes.HasPrefix(bytes.TrimSpace(b), []byte("module github.com/urnetwork/sdk\n")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("run from the sdk repo (go run ./licenses), or from an app repo with go -C ../sdk run ./licenses")
		}
		dir = parent
	}
}

func gitCommit(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func setOf(values []string) map[string]bool {
	m := map[string]bool{}
	for _, v := range values {
		m[v] = true
	}
	return m
}

func sortedApps(apps map[string]bool) []string {
	var out []string
	for _, app := range allApps {
		if apps[app] {
			out = append(out, app)
		}
	}
	return out
}

// License policy
//
// Every app ships the SDK statically (gomobile, cgo, wasm) together with its
// own dependencies, and our code is MPL-2.0. checkPolicy fails generation and
// -check unless every entry's license is one of:
//
//   - permissive or file-level copyleft (policyAllowed), usable anywhere;
//   - LGPL, only for a library the Linux app links dynamically from the
//     system or its AppImage (an extra.yml entry for linux alone), which keeps
//     the LGPL's relinking terms satisfied;
//   - a reviewed proprietary term (policyAcceptedRefs), with the reason it is
//     acceptable.
//
// Anything else fails closed: GPL, AGPL, SSPL, non-commercial or share-alike
// terms, an unrecognized license, or an entry with no identified license. Each
// entry's text is also checked on its own, so a GPL text behind a permissive
// package.json field still fails.

var policyAllowed = map[string]bool{
	"0BSD":          true,
	"Apache-2.0":    true,
	"BlueOak-1.0.0": true,
	"BSD-2-Clause":  true,
	"BSD-3-Clause":  true,
	"BSL-1.0":       true,
	"CC-BY-4.0":     true, // data, with attribution (GeoNames)
	"CC0-1.0":       true,
	"ISC":           true,
	"MIT":           true,
	"MPL-1.1":       true,
	"MPL-2.0":       true, // file-level copyleft, our own license
	"OFL-1.1":       true, // fonts, redistributed unmodified
	"Unlicense":     true,
	"WTFPL":         true,
	"Zlib":          true,
}

var policyLinuxDynamic = map[string]bool{
	"LGPL-2.1-only":     true,
	"LGPL-2.1-or-later": true,
	"LGPL-3.0-only":     true,
	"LGPL-3.0-or-later": true,
}

// reviewed proprietary terms, and why each is acceptable
var policyAcceptedRefs = map[string]string{
	"LicenseRef-Android-SDK": "Google's Android SDK terms; permits distribution in Android apps",
	"LicenseRef-Play-Core-Software-Development-Kit-Terms-of-Service": "Google Play Core terms; permits distribution in Play apps",
	"LicenseRef-Play-Integrity-API-Terms-of-Service":                 "Google Play Integrity terms; permits distribution in Play apps",
	"LicenseRef-MaxMind-GeoLite2-EULA":                               "GeoLite2 EULA; requires the attribution notice, which every app shows",
	"LicenseRef-Public-Domain":                                       "public domain data (Natural Earth)",
	"LicenseRef-Wintun-Prebuilt-Binaries":                            "WireGuard's prebuilt Wintun license; permits redistributing the signed DLL unmodified",
}

// licenses whose own text marks them as strong copyleft, found by the text's
// title so that MPL and LGPL texts, which mention the GPL, do not match
var policyForbiddenTitles = []string{
	"GNU GENERAL PUBLIC LICENSE",
	"GNU AFFERO GENERAL PUBLIC LICENSE",
	"Server Side Public License",
	"Commons Clause",
	"Business Source License",
}

var gplNoticeRe = regexp.MustCompile(`(?i)under the terms of the GNU (Affero )?General Public License as published`)

func checkPolicy(entries []*entry, textOf func(*entry) string) error {
	var problems []string
	for _, e := range entries {
		label := fmt.Sprintf("%s %s %s (%s)", e.Origin, e.Name, e.Version, strings.Join(e.Apps, ","))
		if e.Spdx == "" {
			problems = append(problems, label+": no license identified; read the text, then set spdx for it in extra.yml or teach detectSpdx")
			continue
		}
		linuxDynamic := e.Origin == "extra" && slices.Equal(e.Apps, []string{"linux"})
		if ok, bad := policyExpressionAllowed(e.Spdx, linuxDynamic); !ok {
			problems = append(problems, fmt.Sprintf("%s: %s is not allowed (%s)", label, e.Spdx, strings.Join(bad, ", ")))
		}
		text := strings.Join(strings.Fields(textOf(e)), " ")
		// the license's own title opens the text
		head := strings.ToUpper(text[:min(len(text), 80)])
		for _, title := range policyForbiddenTitles {
			if strings.HasPrefix(head, strings.ToUpper(title)) {
				problems = append(problems, fmt.Sprintf("%s: the license text is %s", label, title))
			}
		}
		// or a GPL notice ("... under the terms of the GNU General Public
		// License as published by ...") heads a file that has no title
		if m := gplNoticeRe.FindString(text); m != "" {
			problems = append(problems, fmt.Sprintf("%s: the license text carries a GPL notice (%q)", label, m))
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("license policy (see checkPolicy in licenses/main.go):\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// policyExpressionAllowed evaluates an SPDX expression: an OR is allowed when
// any side is, an AND when every side is. It returns the disallowed ids.
func policyExpressionAllowed(expression string, linuxDynamic bool) (bool, []string) {
	tokens := strings.Fields(strings.NewReplacer("(", " ( ", ")", " ) ").Replace(expression))
	pos := 0
	var bad []string
	var parseOr func() bool
	parseAtom := func() bool {
		if pos >= len(tokens) {
			bad = append(bad, "<empty>")
			return false
		}
		token := tokens[pos]
		pos += 1
		if token == "(" {
			ok := parseOr()
			if pos < len(tokens) && tokens[pos] == ")" {
				pos += 1
			}
			return ok
		}
		// an exception never widens what the base license allows here
		if pos+1 < len(tokens) && tokens[pos] == "WITH" {
			pos += 2
		}
		id := strings.TrimSuffix(token, "+")
		switch {
		case policyAllowed[id]:
			return true
		case linuxDynamic && policyLinuxDynamic[id]:
			return true
		case policyAcceptedRefs[id] != "":
			return true
		}
		bad = append(bad, token)
		return false
	}
	parseAnd := func() bool {
		ok := parseAtom()
		for pos < len(tokens) && tokens[pos] == "AND" {
			pos += 1
			// evaluate every side so each bad id is reported
			ok = parseAtom() && ok
		}
		return ok
	}
	parseOr = func() bool {
		ok := parseAnd()
		for pos < len(tokens) && tokens[pos] == "OR" {
			pos += 1
			ok = parseAnd() || ok
		}
		return ok
	}
	ok := parseOr()
	if ok {
		bad = nil
	}
	return ok, bad
}
