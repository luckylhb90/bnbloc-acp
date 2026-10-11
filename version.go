package main

import (
	"encoding/json"
	"net/http"
)

// Build identity, stamped at link time:
//
//	go build -ldflags "-X main.version=v0.1.0 -X main.commit=$(git rev-parse HEAD) -X main.builtAt=$(date -u +%FT%TZ)"
var (
	version = "dev"
	commit  = ""
	builtAt = ""
)

// serveVersion answers GET /version with the build identity.
func serveVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"service": "bnbloc-acp", "version": version, "commit": commit, "builtAt": builtAt})
}
