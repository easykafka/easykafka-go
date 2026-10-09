// Package architecture_test is the architecture guard: it parses the import
// lines of every library package and checks the rules below, which keep the
// subscriber and the publisher laid out the same way and their boundaries
// where the layout puts them.
//
// The library is every non-test .go file of the module outside tests/ and
// examples/. Each library package belongs to a side or to the shared list:
//
//   - the subscribe side: subscribe/ and everything under internal/subscribe/;
//   - the publish side: publish/ and everything under internal/publish/;
//   - shared: the root package (documentation only) and internal/logcode.
//
// The rules, numbered as the tests that prove each can fail:
//
//  1. confluent-kafka-go is imported only by internal/subscribe/subscribedriver
//     and internal/publish/publishdriver. Each side reaches librdkafka through
//     its one driver, behind an interface, so confluent types never reach the
//     public API and each side's logic is unit-tested against a fake driver.
//
//  2. The publish side never imports the subscribe side: nothing under
//     publish/ or internal/publish/ imports subscribe/ or anything under
//     internal/subscribe/. The publisher stands on its own; the subscriber's
//     retry strategy is one of its users, not part of it.
//
//  3. The subscribe side reaches the publisher through its public API, the
//     publish package, as any other user does. One exception:
//     internal/subscribe/strategy imports internal/publish/publishdriver, for
//     the type of its WithProducerFactory test seam and for
//     publishdriver.ManagedKeyReason, the list of the keys the publisher
//     manages, which publish does not expose. Any other import of
//     internal/publish/... from the subscribe side breaks the rule.
//
//  4. A driver never imports its public package: subscribedriver never imports
//     subscribe, publishdriver never imports publish. The dependency runs one
//     way, from the public package down to its driver.
//
//  5. internal/logcode imports nothing from the module, and the root package
//     imports nothing at all. Both are shared by both sides, so they must not
//     pull either side in; the root package is documentation only.
//
//  6. Every library package is assigned to a side or to the shared list. A
//     new package that is neither fails until it is placed, so the rules
//     above keep covering code added later.
//
// A violation is reported as one failed assertion per offending import, naming
// the rule, the importing package and the import, for example:
//
//	rule 1, confluent-kafka-go only in the drivers: internal/subscribe/strategy
//	imports github.com/confluentinc/confluent-kafka-go/v2/kafka
//
// The check is syntactic: it reads import lines with go/parser, with no type
// checking. A rule added here goes into this comment in the same change.
//
// This file holds the module walk, the parsing, the rules and their tests on
// purpose, against the module's rule that test files contain tests only: the
// guard reads best in one place.
package architecture_test

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	modulePath = "github.com/easykafka/easykafka-go"
	confluent  = "github.com/confluentinc/confluent-kafka-go"

	// Library packages, as module-relative paths. The root package is "".
	rootPackage      = ""
	logcodePackage   = "internal/logcode"
	subscribePackage = "subscribe"
	subscribeDriver  = "internal/subscribe/subscribedriver"
	subscribeInside  = "internal/subscribe"
	subscribeRetry   = "internal/subscribe/strategy"
	publishPackage   = "publish"
	publishDriver    = "internal/publish/publishdriver"
	publishInside    = "internal/publish"
)

// side is what a library package belongs to.
type side string

const (
	sideSubscribe  side = "subscribe"
	sidePublish    side = "publish"
	sideShared     side = "shared"
	sideUnassigned side = ""
)

// edge is one import: package imports imported. A package is module-relative
// ("" for the root package). imported is module-relative too when it is in the
// module, and a full import path otherwise.
type edge struct {
	pkg      string
	imported string
	inModule bool
}

// graph is what the guard knows about the library: every package, and every
// import of every package.
type graph struct {
	packages []string
	edges    []edge
}

// rule is one of the numbered rules of the comment above. check returns one
// line per violation; none means the rule holds.
type rule struct {
	number int
	name   string
	check  func(g graph) []string
}

var rules = []rule{
	{1, "confluent-kafka-go only in the drivers", confluentOnlyInDrivers},
	{2, "the publish side never imports the subscribe side", publishNeverImportsSubscribe},
	{3, "the subscribe side reaches the publisher through its public API", subscribeUsesPublicPublishAPI},
	{4, "a driver never imports its public package", driverNeverImportsItsPublicPackage},
	{5, "logcode and the root package import nothing from the module", sharedPackagesImportNothing},
	{6, "every package is assigned to a side or shared", everyPackageAssigned},
}

// sideOf assigns a library package to its side.
func sideOf(pkg string) side {
	switch {
	case pkg == subscribePackage || within(pkg, subscribeInside):
		return sideSubscribe
	case pkg == publishPackage || within(pkg, publishInside):
		return sidePublish
	case pkg == rootPackage || pkg == logcodePackage:
		return sideShared
	default:
		return sideUnassigned
	}
}

// within reports whether pkg is dir or a package below it.
func within(pkg, dir string) bool {
	return pkg == dir || strings.HasPrefix(pkg, dir+"/")
}

// confluentOnlyInDrivers is rule 1.
func confluentOnlyInDrivers(g graph) []string {
	var violations []string
	for _, e := range g.edges {
		if e.inModule || !within(e.imported, confluent) {
			continue
		}
		if e.pkg != subscribeDriver && e.pkg != publishDriver {
			violations = append(violations, e.String())
		}
	}
	return violations
}

// publishNeverImportsSubscribe is rule 2.
func publishNeverImportsSubscribe(g graph) []string {
	var violations []string
	for _, e := range g.edges {
		if e.inModule && sideOf(e.pkg) == sidePublish && sideOf(e.imported) == sideSubscribe {
			violations = append(violations, e.String())
		}
	}
	return violations
}

// subscribeUsesPublicPublishAPI is rule 3, with its one exception.
func subscribeUsesPublicPublishAPI(g graph) []string {
	var violations []string
	for _, e := range g.edges {
		if !e.inModule || sideOf(e.pkg) != sideSubscribe || !within(e.imported, publishInside) {
			continue
		}
		if e.pkg == subscribeRetry && e.imported == publishDriver {
			continue // the exception: the test seam's type and ManagedKeyReason
		}
		violations = append(violations, e.String())
	}
	return violations
}

// driverNeverImportsItsPublicPackage is rule 4.
func driverNeverImportsItsPublicPackage(g graph) []string {
	publicOf := map[string]string{subscribeDriver: subscribePackage, publishDriver: publishPackage}
	var violations []string
	for _, e := range g.edges {
		if public, isDriver := publicOf[e.pkg]; isDriver && e.inModule && e.imported == public {
			violations = append(violations, e.String())
		}
	}
	return violations
}

// sharedPackagesImportNothing is rule 5.
func sharedPackagesImportNothing(g graph) []string {
	var violations []string
	for _, e := range g.edges {
		if e.pkg == rootPackage || (e.pkg == logcodePackage && e.inModule) {
			violations = append(violations, e.String())
		}
	}
	return violations
}

// everyPackageAssigned is rule 6.
func everyPackageAssigned(g graph) []string {
	var violations []string
	for _, pkg := range g.packages {
		if sideOf(pkg) == sideUnassigned {
			violations = append(violations, fmt.Sprintf("%s is on neither side nor shared", displayName(pkg)))
		}
	}
	return violations
}

func (e edge) String() string {
	imported := e.imported
	if e.inModule {
		imported = displayName(imported)
	}
	return fmt.Sprintf("%s imports %s", displayName(e.pkg), imported)
}

// displayName names a module-relative package, the root one included.
func displayName(pkg string) string {
	if pkg == rootPackage {
		return "the root package"
	}
	return pkg
}

// moduleRoot walks up from the test's working directory to the module root.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "walked past the filesystem root without finding go.mod")
		dir = parent
	}
}

// scanModule parses the imports of every library file: every non-test .go file
// outside tests/, examples/, bin/ and .git/.
func scanModule(t *testing.T) graph {
	t.Helper()
	root := moduleRoot(t)
	fset := token.NewFileSet()
	packages := map[string]bool{}
	edges := map[edge]bool{}

	err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if slices.Contains([]string{"tests", "examples", "bin", ".git"}, rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}

		parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		pkg := path.Dir(rel)
		if pkg == "." {
			pkg = rootPackage
		}
		packages[pkg] = true
		for _, spec := range parsed.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			edges[toEdge(pkg, imported)] = true
		}
		return nil
	})
	require.NoError(t, err)

	g := graph{}
	for pkg := range packages {
		g.packages = append(g.packages, pkg)
	}
	for e := range edges {
		g.edges = append(g.edges, e)
	}
	slices.Sort(g.packages)
	slices.SortFunc(g.edges, func(a, b edge) int { return strings.Compare(a.String(), b.String()) })
	return g
}

// toEdge records that pkg imports the import path imported.
func toEdge(pkg, imported string) edge {
	if imported == modulePath {
		return edge{pkg: pkg, imported: rootPackage, inModule: true}
	}
	if relative, ok := strings.CutPrefix(imported, modulePath+"/"); ok {
		return edge{pkg: pkg, imported: relative, inModule: true}
	}
	return edge{pkg: pkg, imported: imported}
}

// TestArchitecture runs every rule on the module.
func TestArchitecture(t *testing.T) {
	g := scanModule(t)

	// The guard proves nothing if the walk found nothing, or no longer finds
	// what the rules are about: the drivers and the one allowed crossing.
	for _, pkg := range []string{rootPackage, logcodePackage, subscribePackage, subscribeDriver, publishPackage, publishDriver} {
		require.Contains(t, g.packages, pkg, "the module walk did not find %s; has it moved?", displayName(pkg))
	}
	require.Contains(t, g.edges, edge{pkg: subscribeRetry, imported: publishDriver, inModule: true},
		"rule 3's exception no longer exists; remove it from the guard and its comment")

	for _, r := range rules {
		for _, violation := range r.check(g) {
			assert.Failf(t, "architecture rule broken", "rule %d, %s: %s", r.number, r.name, violation)
		}
	}
}

// moduleEdge and externalEdge build the synthetic graphs of the tests below.
func moduleEdge(pkg, imported string) edge { return edge{pkg: pkg, imported: imported, inModule: true} }

func externalEdge(pkg, imported string) edge { return edge{pkg: pkg, imported: imported} }

// assertRuleCanFail checks a rule on synthetic graphs: each of allowed must
// pass it, and each of broken must break it.
func assertRuleCanFail(t *testing.T, check func(graph) []string, allowed, broken []edge) {
	t.Helper()
	for _, e := range allowed {
		assert.Empty(t, check(graph{edges: []edge{e}}), "allowed: %s", e)
	}
	for _, e := range broken {
		assert.Equal(t, []string{e.String()}, check(graph{edges: []edge{e}}), "broken: %s", e)
	}
}

func TestRule1ConfluentOnlyInDriversCanFail(t *testing.T) {
	kafka := confluent + "/v2/kafka"
	assertRuleCanFail(t, confluentOnlyInDrivers,
		[]edge{externalEdge(subscribeDriver, kafka), externalEdge(publishDriver, kafka), externalEdge(subscribePackage, "context")},
		[]edge{externalEdge(subscribePackage, kafka), externalEdge(subscribeRetry, kafka), externalEdge(publishPackage, kafka),
			externalEdge(logcodePackage, kafka), externalEdge(rootPackage, kafka)},
	)
}

func TestRule2PublishNeverImportsSubscribeCanFail(t *testing.T) {
	assertRuleCanFail(t, publishNeverImportsSubscribe,
		[]edge{moduleEdge(publishPackage, publishDriver), moduleEdge(publishPackage, logcodePackage),
			moduleEdge(subscribeRetry, publishPackage)},
		[]edge{moduleEdge(publishPackage, subscribePackage), moduleEdge(publishPackage, "internal/subscribe/types"),
			moduleEdge(publishDriver, subscribeDriver)},
	)
}

func TestRule3SubscribeUsesPublicPublishAPICanFail(t *testing.T) {
	assertRuleCanFail(t, subscribeUsesPublicPublishAPI,
		[]edge{moduleEdge(subscribeRetry, publishPackage), moduleEdge(subscribeRetry, publishDriver),
			moduleEdge(subscribePackage, subscribeRetry)},
		[]edge{moduleEdge(subscribePackage, publishDriver), moduleEdge(subscribeDriver, publishDriver),
			moduleEdge(subscribeRetry, "internal/publish/other")},
	)
}

func TestRule4DriverNeverImportsItsPublicPackageCanFail(t *testing.T) {
	assertRuleCanFail(t, driverNeverImportsItsPublicPackage,
		[]edge{moduleEdge(subscribePackage, subscribeDriver), moduleEdge(publishPackage, publishDriver),
			moduleEdge(subscribeDriver, "internal/subscribe/types")},
		[]edge{moduleEdge(subscribeDriver, subscribePackage), moduleEdge(publishDriver, publishPackage)},
	)
}

func TestRule5SharedPackagesImportNothingCanFail(t *testing.T) {
	assertRuleCanFail(t, sharedPackagesImportNothing,
		[]edge{externalEdge(logcodePackage, "fmt"), moduleEdge(subscribePackage, logcodePackage)},
		[]edge{moduleEdge(logcodePackage, subscribeDriver), moduleEdge(logcodePackage, publishPackage),
			moduleEdge(rootPackage, subscribePackage), externalEdge(rootPackage, "fmt")},
	)
}

func TestRule6EveryPackageAssignedCanFail(t *testing.T) {
	for _, pkg := range []string{rootPackage, logcodePackage, subscribePackage, subscribeRetry, publishPackage, publishDriver} {
		assert.Empty(t, everyPackageAssigned(graph{packages: []string{pkg}}), "assigned: %s", displayName(pkg))
	}
	for _, pkg := range []string{"internal/engine", "strategy", "internal/other", "subscriber", "publisher/x"} {
		assert.Equal(t, []string{pkg + " is on neither side nor shared"},
			everyPackageAssigned(graph{packages: []string{pkg}}), "unassigned: %s", pkg)
	}
}
