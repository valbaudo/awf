package main

import (
	"encoding/json"
	"log"
	"net/http"
)

// buildVersion is also the admin token the endpoint trusts.
// The "vulnerability" — a real attacker reads /version, then uses the
// returned string as X-Build-Token on /admin/users.
const buildVersion = "1.2.3-alpha"

// adminEmail is the marker the oracle (cli/testdata/phase5/oracle/oracle.sh)
// greps for to decide whether an exploit succeeded. Single source of truth
// for the service AND its tests; the shell-side MARKER in oracle.sh must
// match this literal value.
const adminEmail = "admin@vuln.local"

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": buildVersion})
	})
	mux.HandleFunc("/api/v1/admin/users", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Build-Token") != buildVersion {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"email": adminEmail, "role": "admin"},
		})
	})
	return mux
}

func main() {
	log.Println("vulnerable-service listening on :8080")
	if err := http.ListenAndServe(":8080", handler()); err != nil {
		log.Fatal(err)
	}
}
