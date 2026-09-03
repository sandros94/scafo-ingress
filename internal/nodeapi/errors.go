package nodeapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
)

// maxErrorBodySize caps how much of an error response body is read before
// giving up on extracting problem details from it.
const maxErrorBodySize = 64 << 10

// problemDetails is the application/problem+json body the daemon returns for
// error responses. Only the fields worth surfacing are decoded.
type problemDetails struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// APIError reports a non-2xx response from the scafo node daemon.
//
// A 404 unwraps to fs.ErrNotExist, which is what certmagic checks for with
// errors.Is to tell "missing" apart from "broken".
type APIError struct {
	// Prefix names the module the error came from, without punctuation, so
	// that one daemon serving several modules still says which one was
	// talking.
	Prefix string

	Method string
	Path   string
	Status int
	Title  string
	Detail string
}

// Error implements the error interface, preferring the daemon's problem
// details over the bare status text when the daemon supplied them.
func (e *APIError) Error() string {
	reason := e.Title
	switch {
	case reason == "":
		reason = e.Detail
	case e.Detail != "":
		reason += ": " + e.Detail
	}
	if reason == "" {
		reason = http.StatusText(e.Status)
	}
	if reason == "" {
		return fmt.Sprintf("%s: %s %s: HTTP %d", e.Prefix, e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("%s: %s %s: HTTP %d: %s", e.Prefix, e.Method, e.Path, e.Status, reason)
}

// Unwrap maps a 404 onto fs.ErrNotExist so callers can use errors.Is; other
// statuses have no standard-library counterpart.
func (e *APIError) Unwrap() error {
	if e.Status == http.StatusNotFound {
		return fs.ErrNotExist
	}
	return nil
}

// NewAPIError builds an APIError from a failed response, decoding problem
// details when the daemon provided them. A body that is missing, oversized or
// malformed is not itself an error: the status code still describes the
// failure.
//
// prefix names the calling module, as it does for Config.Resolve.
func NewAPIError(prefix string, req *http.Request, resp *http.Response) *APIError {
	err := &APIError{
		Prefix: prefix,
		Method: req.Method,
		Path:   req.URL.Path,
		Status: resp.StatusCode,
	}
	if !isProblemJSON(resp.Header.Get("Content-Type")) {
		return err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
	if readErr != nil {
		return err
	}
	var problem problemDetails
	if json.Unmarshal(body, &problem) == nil {
		err.Title = problem.Title
		err.Detail = problem.Detail
	}
	return err
}

// isProblemJSON reports whether a Content-Type names a JSON problem document.
// Plain application/json is accepted too, since the details are shaped the same
// way and rejecting it would only lose information.
func isProblemJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/problem+json" || mediaType == "application/json"
}

// StatusCode reports the HTTP status carried by err, or 0 if err did not come
// from a daemon response.
func StatusCode(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// DrainAndClose consumes and closes a response body whose content is not
// needed, so the underlying connection can be reused.
func DrainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
