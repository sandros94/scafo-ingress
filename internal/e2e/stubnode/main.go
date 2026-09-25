// Command stubnode is a scafo node daemon stand-in for the integration proof.
//
// It serves exactly the two surfaces the Caddy modules in this repository
// talk to -- the routing document and the TLS storage API -- and, on a second
// port, an application backend for the routing document to point at. Its
// storage is a map in memory, and it exposes that map on an unauthenticated
// debug route so a test can see WHICH node a certificate landed in, which is
// the whole point of running two of them.
//
// Everything about it comes from the environment:
//
//	FLEET       the name it answers with, for the backend's body
//	TOKEN       the bearer token it admits
//	ROUTES      the routing document, verbatim JSON
//	NODE_ADDR   the address to serve the node API on (default :7400)
//	APP_ADDR    the address to serve the application backend on (default :8080)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// store is the in-memory TLS keyspace this node pretends to own.
type store struct {
	mu    sync.Mutex
	items map[string][]byte
	locks map[string]struct{}
}

func newStore() *store {
	return &store{items: map[string][]byte{}, locks: map[string]struct{}{}}
}

func (s *store) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.items))
	for key := range s.items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func main() {
	fleet := env("FLEET", "stub")
	token := env("TOKEN", "")
	routes := env("ROUTES", `{"v":1,"routes":[]}`)
	nodeAddr := env("NODE_ADDR", ":7400")
	appAddr := env("APP_ADDR", ":8080")

	keys := newStore()

	node := http.NewServeMux()

	// The debug surface, deliberately unauthenticated and deliberately only
	// on this stub: it is how the test sees which node holds which key.
	node.HandleFunc("GET /_debug/keys", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, keys.keys())
	})
	node.HandleFunc("GET /_debug/fleet", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"fleet": fleet})
	})

	node.HandleFunc("GET /v1/ingress/routes", func(w http.ResponseWriter, r *http.Request) {
		if !admit(w, r, token) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, routes)
	})

	node.HandleFunc("GET /v1/storage/list", func(w http.ResponseWriter, r *http.Request) {
		if !admit(w, r, token) {
			return
		}
		prefix := r.URL.Query().Get("prefix")
		keys.mu.Lock()
		var found []string
		for key := range keys.items {
			if key == prefix || strings.HasPrefix(key, prefix+"/") {
				found = append(found, key)
			}
		}
		keys.mu.Unlock()
		if len(found) == 0 {
			// A listing of nothing is a 404, not an empty array: CertMagic
			// needs "not found" to be distinguishable.
			problem(w, http.StatusNotFound, "not-found", "nothing under that prefix")
			return
		}
		sort.Strings(found)
		writeJSON(w, found)
	})

	node.HandleFunc("GET /v1/storage/stat", func(w http.ResponseWriter, r *http.Request) {
		if !admit(w, r, token) {
			return
		}
		key := r.URL.Query().Get("key")
		keys.mu.Lock()
		value, ok := keys.items[key]
		keys.mu.Unlock()
		if !ok {
			problem(w, http.StatusNotFound, "not-found", "no such key")
			return
		}
		now := time.Now().UTC()
		writeJSON(w, map[string]any{
			"key":        key,
			"modified":   now.Format(time.RFC3339),
			"size":       len(value),
			"isTerminal": true,
		})
	})

	node.HandleFunc("/v1/storage/objects/", func(w http.ResponseWriter, r *http.Request) {
		if !admit(w, r, token) {
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/v1/storage/objects/")
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				problem(w, http.StatusBadRequest, "invalid-input", err.Error())
				return
			}
			keys.mu.Lock()
			keys.items[key] = body
			keys.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			keys.mu.Lock()
			value, ok := keys.items[key]
			keys.mu.Unlock()
			if !ok {
				problem(w, http.StatusNotFound, "not-found", "no such key")
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(value)
		case http.MethodDelete:
			keys.mu.Lock()
			removed := false
			for existing := range keys.items {
				if existing == key || strings.HasPrefix(existing, key+"/") {
					delete(keys.items, existing)
					removed = true
				}
			}
			keys.mu.Unlock()
			if !removed {
				problem(w, http.StatusNotFound, "not-found", "no such key")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			problem(w, http.StatusMethodNotAllowed, "method-not-allowed", r.Method)
		}
	})

	node.HandleFunc("/v1/storage/locks/", func(w http.ResponseWriter, r *http.Request) {
		if !admit(w, r, token) {
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/v1/storage/locks/")
		switch r.Method {
		case http.MethodPost:
			keys.mu.Lock()
			_, held := keys.locks[name]
			if !held {
				keys.locks[name] = struct{}{}
			}
			keys.mu.Unlock()
			if held {
				problem(w, http.StatusConflict, "cas-conflict", "the lock is held")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			keys.mu.Lock()
			delete(keys.locks, name)
			keys.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			problem(w, http.StatusMethodNotAllowed, "method-not-allowed", r.Method)
		}
	})

	app := http.NewServeMux()
	app.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "fleet=%s host=%s path=%s\n", fleet, r.Host, r.URL.Path)
	})

	go func() {
		log.Printf("stubnode %s: application backend on %s", fleet, appAddr)
		log.Fatal(http.ListenAndServe(appAddr, app))
	}()
	log.Printf("stubnode %s: node API on %s", fleet, nodeAddr)
	log.Fatal(http.ListenAndServe(nodeAddr, node))
}

// admit checks the ingress bearer token, the way a real node does.
func admit(w http.ResponseWriter, r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	if r.Header.Get("Authorization") == "Bearer "+token {
		return true
	}
	problem(w, http.StatusUnauthorized, "auth-invalid", "the bearer token is not admitted here")
	return false
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func problem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"title":  title,
		"detail": detail,
		"status": status,
	})
}
