package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVersionEndpoint(t *testing.T) {
	srv := httptest.NewServer(handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"version":"1.2.3-alpha"`) {
		t.Errorf("body = %s; want JSON with version=1.2.3-alpha", body)
	}
}

func TestAdminEndpointRejectsMissingToken(t *testing.T) {
	srv := httptest.NewServer(handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/admin/users")
	if err != nil {
		t.Fatalf("GET /admin/users: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("status = %d; want 403 (no token)", resp.StatusCode)
	}
}

func TestAdminEndpointRejectsArbitraryToken(t *testing.T) {
	srv := httptest.NewServer(handler())
	defer srv.Close()
	req, err := http.NewRequest("GET", srv.URL+"/api/v1/admin/users", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Build-Token", "AAAAAA")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /admin/users: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("status = %d; want 403 (arbitrary token)", resp.StatusCode)
	}
}

func TestAdminEndpointAcceptsVersionToken(t *testing.T) {
	srv := httptest.NewServer(handler())
	defer srv.Close()
	req, err := http.NewRequest("GET", srv.URL+"/api/v1/admin/users", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Build-Token", "1.2.3-alpha")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /admin/users: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d; want 200 (version-as-token vulnerability)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	if !strings.Contains(string(body), "admin@vuln.local") {
		t.Errorf("body = %s; want JSON containing admin@vuln.local", body)
	}
}
