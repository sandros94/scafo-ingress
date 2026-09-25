package fleets

import (
	"encoding/json"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"go.uber.org/zap"
)

// parseDocument decodes and indexes a document the way Fetch does, and gives
// it the content hash a fetched one would carry.
func parseDocument(t *testing.T, raw string) *Document {
	t.Helper()
	var doc Document
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decoding document: %v", err)
	}
	doc.index()
	doc.hash = hashBytes([]byte(raw))
	return &doc
}

// testApp builds an app whose fleets hold the given documents and talk to
// nothing. The fleets are in the order given, which is the declaration order
// the conflict rules depend on.
//
// Each entry is a fleet name and the raw routing document it holds; an empty
// document means the fleet's daemon has not answered yet.
type testFleet struct {
	name     string
	document string
	plain    bool // http_redirect off
}

func testApp(t *testing.T, entries ...testFleet) *App {
	t.Helper()
	app := &App{
		logger:    zap.NewNop(),
		claims:    map[claimKey]string{},
		conflicts: map[claimKey]string{},
		byName:    map[string]*Fleet{},
	}
	for _, entry := range entries {
		fleet := &Fleet{Name: entry.name}
		if entry.plain {
			off := false
			fleet.HTTPRedirect = &off
		}
		fleet.routes = NewRoutes(nil, "", DefaultPrefer, 0, zap.NewNop())
		if entry.document != "" {
			publish(t, fleet, entry.document)
		}
		app.fleets = append(app.fleets, fleet)
		app.byName[fleet.Name] = fleet
	}
	return app
}

// publish gives a fleet a document, the way a successful fetch would.
func publish(t *testing.T, fleet *Fleet, raw string) {
	t.Helper()
	fleet.routes.current.Store(parseDocument(t, raw))
	fleet.routes.generation.Add(1)
}

// withdraw takes a fleet's document away, the way nothing ever does -- a
// daemon that stops answering keeps its last document -- but which is the
// shortest way to test a fleet that stops publishing a host.
func withdraw(t *testing.T, fleet *Fleet) {
	t.Helper()
	fleet.routes.current.Store(nil)
	fleet.routes.generation.Add(1)
}

// dials reports the addresses of upstreams, which is all a lookup result is.
func dials(upstreams []*reverseproxy.Upstream) []string {
	addresses := make([]string, 0, len(upstreams))
	for _, upstream := range upstreams {
		addresses = append(addresses, upstream.Dial)
	}
	return addresses
}

// equalStrings compares two slices for a test's purposes.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
