package fleets

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/sandros94/scafo-ingress/internal/nodeapi"
)

// DefaultPollInterval is how long a fleet's poller waits between attempts when
// poll_interval is not configured.
const DefaultPollInterval = 5 * time.Second

// DefaultRequestTimeout bounds a single fetch of a routing document. The
// daemon is normally colocated, so anything slower than this is a fault rather
// than latency, and giving up returns the poller to its normal cadence.
const DefaultRequestTimeout = 10 * time.Second

// DefaultPrefer is the ordering a poller asks for when prefer is not
// configured: "self" puts the answering node's own upstreams first, which is
// what keeps a colocated Caddy proxying locally.
const DefaultPrefer = "self"

// PreferNone is the one prefer value this package reads rather than forwards.
// It is not a word the daemon knows: it suppresses the query parameter
// entirely, and an absent parameter is how the daemon is asked to leave the
// fleet's own order alone. Every other value -- "self", or the id of a node --
// goes over as written.
const PreferNone = "none"

// maxDocumentSize caps how much of a routing document is read. A daemon that
// answers with something unbounded is a fault, and reading it into memory
// would be this process's problem rather than that daemon's.
const maxDocumentSize = 32 << 20

// Routes is a live copy of one fleet's routing document: it fetches the
// document from that fleet's node daemon, keeps the last one that parsed, and
// hands it out.
//
// A Routes value is safe for concurrent readers -- Document is an atomic load
// -- while fetching is single-threaded by construction: the first fetch happens
// before the poller is started, and the poller never overlaps itself.
type Routes struct {
	// base is the daemon's base URL with any trailing slash removed, so that
	// request paths can be appended directly.
	base string

	// token is the bearer token presented to the daemon.
	token string

	// prefer is the ordering asked of the daemon, sent verbatim as the prefer
	// query parameter. PreferNone and the empty string send no parameter at
	// all; see requestURL.
	prefer string

	// client performs the HTTP requests. It is dedicated to this Routes so
	// that timeouts and connection pooling are not shared with unrelated
	// traffic.
	client *http.Client

	// timeout bounds one fetch.
	timeout time.Duration

	// logger reports fetch failures and recoveries. It is replaceable, and
	// atomic, because a Routes outlives the configuration that created it: the
	// poller is shared between handlers and survives reloads, so each
	// configuration that adopts it lends it its own logger rather than leaving
	// it writing to the log of a configuration that has been unloaded. See
	// SetLogger.
	logger atomic.Pointer[zap.Logger]

	// current holds the last document that parsed, or nil before the first
	// one arrives. It is the only field a request reads.
	current atomic.Pointer[Document]

	// generation counts the documents whose CONTENT differed from the one
	// before. A fetch that returns the same bytes does not bump it, which is
	// what lets the cross-fleet index rebuild -- and a host conflict be logged
	// -- once per real change instead of once per poll.
	generation atomic.Uint64

	// changed is called, off the request path, whenever generation moves. It
	// is how the app learns that its index is stale.
	changed func()

	// failing records whether the last fetch failed, so that a daemon that is
	// down logs once per state change instead of once per tick. Only the
	// fetch path touches it, and that path never runs concurrently with
	// itself.
	failing bool
}

// NewRoutes returns a Routes that polls a scafo node daemon at endpoint,
// authenticating with the given bearer token and asking for the given
// ordering.
//
// endpoint may include a path prefix (e.g. https://host/scafo); the protocol
// path is appended to it. A nil endpoint or an empty token is a programming
// error and yields a Routes whose every fetch fails. A nil logger is replaced
// by one that discards.
func NewRoutes(endpoint *url.URL, token, prefer string, timeout time.Duration, logger *zap.Logger) *Routes {
	if logger == nil {
		logger = zap.NewNop()
	}
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	routes := &Routes{
		base:    BaseURL(endpoint),
		token:   token,
		prefer:  prefer,
		timeout: timeout,
		client: &http.Client{
			Timeout: timeout,
		},
	}
	routes.logger.Store(logger)
	return routes
}

// BaseURL reduces an endpoint to the form protocol paths are appended to: its
// string with any trailing separator removed.
func BaseURL(endpoint *url.URL) string {
	if endpoint == nil {
		return ""
	}
	return strings.TrimSuffix(endpoint.String(), "/")
}

// SetLogger hands the poller the logger to report fetch failures and
// recoveries to, replacing the one it holds. A nil logger is ignored, so a
// caller without one leaves the poller's own in place rather than silencing it.
func (r *Routes) SetLogger(logger *zap.Logger) {
	if logger != nil {
		r.logger.Store(logger)
	}
}

// OnChange registers the callback invoked whenever a fetch produced a document
// whose content differs from the previous one. It is called from the fetching
// goroutine and must not block for long.
func (r *Routes) OnChange(fn func()) {
	r.changed = fn
}

// Document returns the last routing document that parsed, or nil when none has
// yet. Callers may hold onto it: a later fetch replaces the pointer rather than
// editing what a request is already reading.
func (r *Routes) Document() *Document {
	return r.current.Load()
}

// Generation counts the documents whose content differed from the one before.
// A reader that remembers it can tell, with one atomic load, whether anything
// it derived from this fleet is still current.
func (r *Routes) Generation() uint64 {
	return r.generation.Load()
}

// CloseIdleConnections drops connections to the daemon that are being kept
// alive but are not in use. It is meant for teardown; the Routes remains
// usable afterwards.
func (r *Routes) CloseIdleConnections() {
	if r.client != nil {
		r.client.CloseIdleConnections()
	}
}

// requestURL builds the routes URL, carrying the configured ordering as the
// prefer parameter unless there is none to ask for.
//
// PreferNone and an unset value both leave the parameter off rather than
// sending it empty, because the daemon reads an absent parameter as "the
// fleet's own order" -- so the way to ask for that is not to ask.
func (r *Routes) requestURL() string {
	target := r.base + "/v1/ingress/routes"
	if r.prefer == "" || r.prefer == PreferNone {
		return target
	}
	return target + "?" + url.Values{"prefer": []string{r.prefer}}.Encode()
}

// Fetch retrieves and parses the routing document without publishing it.
func (r *Routes) Fetch(ctx context.Context) (*Document, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.requestURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: building routes request: %w", errPrefix, err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/json")

	client := r.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s %s: %w", errPrefix, req.Method, req.URL.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer nodeapi.DrainAndClose(resp)
		return nil, nodeapi.NewAPIError(errPrefix, req, resp)
	}
	defer resp.Body.Close()

	// The body is read whole rather than streamed into the decoder, because
	// the poller needs its bytes twice: once to decode, and once to tell this
	// answer apart from the previous one. See Document.hash.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentSize))
	if err != nil {
		return nil, fmt.Errorf("%s: reading routing document: %w", errPrefix, err)
	}

	var doc Document
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%s: decoding routing document: %w", errPrefix, err)
	}
	if doc.Version != DocumentVersion {
		return nil, fmt.Errorf("%s: routing document version %d, want %d", errPrefix, doc.Version, DocumentVersion)
	}
	doc.index()
	doc.hash = hashBytes(body)
	return &doc, nil
}

// Refresh fetches the routing document and publishes it if it parsed, keeping
// the previous one otherwise. It reports nothing: a daemon that is briefly
// unreachable is an event to log and outlive, not a failure to propagate to
// whoever happened to call.
//
// A document identical to the one already held is dropped rather than
// published. Nothing downstream can tell the difference, and it is what keeps
// the cross-fleet index from being rebuilt -- and a host conflict from being
// logged -- once per poll.
//
// Failures are logged once per state change rather than once per tick, so a
// daemon that stays down leaves one warning behind and a daemon that comes back
// says so.
func (r *Routes) Refresh(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	doc, err := r.Fetch(fetchCtx)
	if err != nil {
		// A cancelled context is the process going away, not the daemon
		// failing; saying so on the way out would be noise.
		if ctx.Err() != nil {
			return
		}
		if !r.failing {
			r.failing = true
			logger := r.logger.Load()
			if r.current.Load() == nil {
				logger.Warn("fetching the routing document failed and none is known yet; this fleet's hosts will not be routed until its daemon answers",
					zap.Error(err))
			} else {
				logger.Warn("fetching the routing document failed; serving the last one that parsed",
					zap.Error(err))
			}
		}
		return
	}
	if r.failing {
		r.failing = false
		r.logger.Load().Info("the routing document is available again")
	}
	if previous := r.current.Load(); previous != nil && previous.hash == doc.hash {
		return
	}
	r.current.Store(doc)
	r.generation.Add(1)
	if r.changed != nil {
		r.changed()
	}
}

// Run refreshes the routing document every interval until ctx is done.
//
// It waits interval between attempts rather than ticking on a schedule, so a
// slow fetch delays the next one instead of stacking up behind it.
func (r *Routes) Run(ctx context.Context, interval time.Duration) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		r.Refresh(ctx)
		timer.Reset(interval)
	}
}
