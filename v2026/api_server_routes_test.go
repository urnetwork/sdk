package sdk

// The sdk's api urls are checked against the server's route table, so a route
// the server removes fails here instead of 404ing in production (UPGRADE.md S6,
// §4.7). The urls are derived from the sdk sources -- every
// fmt.Sprintf("%s/...", apiUrl) -- rather than a hand-written list, so a new
// call is covered the moment it is written.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// one api url the sdk builds
type sdkApiUrl struct {
	// the format's path, without the query, e.g. "/key/%s/history"
	path     string
	position string
}

// sdkApiUrls scans the non-test go files in sdkDir for urls built on the api
// base url: fmt.Sprintf calls whose format starts with "%s/" and whose first
// argument is apiUrl (a field or a local). Other bases (the network space
// service url, the sn artifact host) are not the api server's.
func sdkApiUrls(t *testing.T, sdkDir string) []*sdkApiUrl {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(sdkDir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	urls := []*sdkApiUrl{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); !ok || selector.Sel.Name != "Sprintf" {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			format, err := strconv.Unquote(literal.Value)
			if err != nil || !strings.HasPrefix(format, "%s/") {
				return true
			}
			base := ""
			switch baseExpr := call.Args[1].(type) {
			case *ast.SelectorExpr:
				base = baseExpr.Sel.Name
			case *ast.Ident:
				base = baseExpr.Name
			}
			if base != "apiUrl" {
				return true
			}
			path, _, _ := strings.Cut(strings.TrimPrefix(format, "%s"), "?")
			urls = append(urls, &sdkApiUrl{
				path:     path,
				position: fset.Position(call.Pos()).String(),
			})
			return true
		})
	}
	return urls
}

// serverRoutePatterns parses the server's api.go route table. Routes are
// regular expressions over the path, e.g. "/key/([^/]+)/history".
func serverRoutePatterns(t *testing.T, routesSource string) map[string]*regexp.Regexp {
	t.Helper()
	routeRe := regexp.MustCompile(`NewRoute\("(?:GET|POST|PUT|DELETE)",\s*"([^"]+)"`)
	routes := map[string]*regexp.Regexp{}
	for _, m := range routeRe.FindAllStringSubmatch(routesSource, -1) {
		pattern, err := regexp.Compile("^" + m[1] + "$")
		if err != nil {
			t.Fatalf("server route %q is not a valid pattern: %s", m[1], err)
		}
		routes[m[1]] = pattern
	}
	if len(routes) == 0 {
		t.Fatal("no routes parsed from server api.go; the route regex is stale")
	}
	return routes
}

// unroutedSdkApiUrls returns the sdk urls no server route serves, with a
// sample value standing in for each format verb.
func unroutedSdkApiUrls(urls []*sdkApiUrl, routes map[string]*regexp.Regexp) []*sdkApiUrl {
	sample := strings.NewReplacer("%s", "x", "%d", "1", "%v", "x")
	unrouted := []*sdkApiUrl{}
	for _, url := range urls {
		samplePath := sample.Replace(url.path)
		routed := false
		for _, pattern := range routes {
			if pattern.MatchString(samplePath) {
				routed = true
				break
			}
		}
		if !routed {
			unrouted = append(unrouted, url)
		}
	}
	return unrouted
}

// The scan must find the sdk's whole api surface, including the payment
// endpoints the old hand-written list missed, or it is checking nothing.
func TestSdkApiUrlScanFindsTheApiSurface(t *testing.T) {
	urls := sdkApiUrls(t, ".")
	if len(urls) < 50 {
		t.Fatalf("scanned %d api urls from the sdk sources, expected the whole api surface", len(urls))
	}
	paths := []string{}
	for _, url := range urls {
		paths = append(paths, url.path)
	}
	for _, path := range []string{
		"/solana/payment-intent",
		"/subscription/stripe/payment-sheet",
		"/subscription/stripe/prices",
		"/subscription/balance",
		"/key/%s/history",
	} {
		if !slices.Contains(paths, path) {
			t.Errorf("the scan did not find %s", path)
		}
	}
}

// A route the server drops is reported, whichever call uses it. Runs on a
// synthetic route table, so it needs no server checkout.
func TestSdkApiUrlMissingFromServerRoutesIsReported(t *testing.T) {
	urls := sdkApiUrls(t, ".")

	var routesSource strings.Builder
	sample := strings.NewReplacer("%s", "([^/]+)", "%d", "([0-9]+)")
	for _, url := range urls {
		if url.path == "/solana/payment-intent" {
			// the route the server dropped
			continue
		}
		fmt.Fprintf(&routesSource, "router.NewRoute(\"POST\", %q, handler),\n", sample.Replace(url.path))
	}

	unrouted := unroutedSdkApiUrls(urls, serverRoutePatterns(t, routesSource.String()))
	if len(unrouted) != 1 || unrouted[0].path != "/solana/payment-intent" {
		reported := []string{}
		for _, url := range unrouted {
			reported = append(reported, url.path)
		}
		t.Fatalf("unrouted = %v, want exactly [/solana/payment-intent]", reported)
	}
}

// Reads the sibling server checkout read-only. Without one it skips with a note
// on stderr, so the skip is never silent.
func TestSdkApiUrlsMatchServerRoutes(t *testing.T) {
	serverApiPath := filepath.Join("..", "server", "api", "api.go")
	routesSource, err := os.ReadFile(serverApiPath)
	if err != nil {
		message := fmt.Sprintf(
			"server route conformance needs the server repo beside the sdk (%s): %s",
			serverApiPath, err,
		)
		fmt.Fprintf(os.Stderr, "NOTE: %s; skipping (check out ../server to run it)\n", message)
		t.Skip(message)
	}
	routes := serverRoutePatterns(t, string(routesSource))

	for _, url := range unroutedSdkApiUrls(sdkApiUrls(t, "."), routes) {
		t.Errorf("SDK calls %s (%s) but the server no longer routes it", url.path, url.position)
	}

	// the deprecated guest-upgrade methods must STAY deprecated while the
	// routes are gone; if the server restores them, this fails to prompt
	// un-deprecating the SDK methods
	for _, route := range []string{"/auth/upgrade-guest", "/auth/upgrade-guest-existing"} {
		if _, ok := routes[route]; ok {
			t.Errorf("server restored %s; un-deprecate the SDK guest-upgrade method", route)
		}
	}
}
