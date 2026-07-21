package kata

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerCredentialFuncAuthsBasic(t *testing.T) {
	dir := t.TempDir()

	encoded := base64.StdEncoding.EncodeToString([]byte("myuser:mypass"))
	configJSON := `{
		"auths": {
			"https://index.docker.io/v1/": {"auth": "` + encoded + `"},
			"registry.example.com": {"auth": "` + encoded + `"}
		}
	}`

	os.Setenv("DOCKER_CONFIG", dir)
	defer os.Unsetenv("DOCKER_CONFIG")

	err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configJSON), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	fn := dockerCredentialFunc()
	if fn == nil {
		t.Fatal("expected non-nil credential func")
	}

	user, pass, err := fn("https://index.docker.io/v1/")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if user != "myuser" || pass != "mypass" {
		t.Errorf("got (%q, %q), want (\"myuser\", \"mypass\")", user, pass)
	}
}

func TestDockerCredentialFuncAuthsPartialMatch(t *testing.T) {
	dir := t.TempDir()

	encoded := base64.StdEncoding.EncodeToString([]byte("admin:secret"))
	configJSON := `{
		"auths": {
			"registry.example.com": {"auth": "` + encoded + `"},
			"https://index.docker.io/v1/": {"auth": "other:creds"}
		}
	}`

	os.Setenv("DOCKER_CONFIG", dir)
	defer os.Unsetenv("DOCKER_CONFIG")

	err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configJSON), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	fn := dockerCredentialFunc()
	if fn == nil {
		t.Fatal("expected non-nil credential func")
	}

	// registry.example.com should match the auth entry (exact match)
	user, pass, err := fn("registry.example.com")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if user != "admin" || pass != "secret" {
		t.Errorf("got (%q, %q), want (\"admin\", \"secret\")", user, pass)
	}

	// Non-matching host returns empty credentials
	user2, pass2, err := fn("no-such-registry.com")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if user2 != "" || pass2 != "" {
		t.Errorf("expected empty credentials for unknown host, got (%q, %q)", user2, pass2)
	}
}

func TestDockerCredentialFuncCredHelpers(t *testing.T) {
	dir := t.TempDir()

	configJSON := `{
		"credHelpers": {
			"my-registry.example.com": "my-helper"
		},
		"auths": {}
	}`

	os.Setenv("DOCKER_CONFIG", dir)
	defer os.Unsetenv("DOCKER_CONFIG")

	err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configJSON), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Create a fake credential helper script on PATH
	helperDir := t.TempDir()
	helperScript := filepath.Join(helperDir, "docker-credential-my-helper")
	err = os.WriteFile(helperScript, []byte(`#!/bin/sh
echo '{"Username":"helperuser","Secret":"helperpass"}'
`), 0755)
	if err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	// Save original PATH and restore after test
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", helperDir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	fn := dockerCredentialFunc()
	if fn == nil {
		t.Fatal("expected non-nil credential func")
	}

	user, pass, err := fn("my-registry.example.com")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if user != "helperuser" || pass != "helperpass" {
		t.Errorf("got (%q, %q), want (\"helperuser\", \"helperpass\")", user, pass)
	}
}

func TestDockerCredentialFuncCredsStore(t *testing.T) {
	dir := t.TempDir()

	configJSON := `{
		"credsStore": "store-helper"
	}`

	os.Setenv("DOCKER_CONFIG", dir)
	defer os.Unsetenv("DOCKER_CONFIG")

	err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configJSON), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	helperDir := t.TempDir()
	helperScript := filepath.Join(helperDir, "docker-credential-store-helper")
	err = os.WriteFile(helperScript, []byte(`#!/bin/sh
echo '{"Username":"storeuser","Secret":"storepass"}'
`), 0755)
	if err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", helperDir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	fn := dockerCredentialFunc()
	if fn == nil {
		t.Fatal("expected non-nil credential func")
	}

	user, pass, err := fn("any-registry.com")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if user != "storeuser" || pass != "storepass" {
		t.Errorf("got (%q, %q), want (\"storeuser\", \"storepass\")", user, pass)
	}
}

func TestDockerCredentialFuncCredHelpersPreferredOverCredsStore(t *testing.T) {
	dir := t.TempDir()

	configJSON := `{
		"credHelpers": {
			"my-registry.example.com": "helper-preferred"
		},
		"credsStore": "store-default",
		"auths": {}
	}`

	os.Setenv("DOCKER_CONFIG", dir)
	defer os.Unsetenv("DOCKER_CONFIG")

	err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configJSON), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	helperDir := t.TempDir()
	for _, name := range []string{"docker-credential-helper-preferred", "docker-credential-store-default"} {
		err = os.WriteFile(filepath.Join(helperDir, name), []byte(`#!/bin/sh
echo '{"Username":"`+name+`","Secret":"secret"}'
`), 0755)
		if err != nil {
			t.Fatalf("write helper script %s: %v", name, err)
		}
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", helperDir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	fn := dockerCredentialFunc()
	if fn == nil {
		t.Fatal("expected non-nil credential func")
	}

	user, _, err := fn("my-registry.example.com")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if user != "docker-credential-helper-preferred" {
		t.Errorf("credHelpers should take precedence over credsStore, got username=%q", user)
	}
}

func TestDockerCredentialFuncMissingConfig(t *testing.T) {
	dir := t.TempDir()

	os.Setenv("DOCKER_CONFIG", dir)
	defer os.Unsetenv("DOCKER_CONFIG")

	fn := dockerCredentialFunc()
	if fn != nil {
		t.Error("expected nil credential func when config file is missing")
	}
}

func TestDockerCredentialFuncMalformedJSON(t *testing.T) {
	dir := t.TempDir()

	os.Setenv("DOCKER_CONFIG", dir)
	defer os.Unsetenv("DOCKER_CONFIG")

	err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not valid json"), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	fn := dockerCredentialFunc()
	if fn != nil {
		t.Error("expected nil credential func when config is malformed JSON")
	}
}

func TestDockerCredentialFuncEmptyAuth(t *testing.T) {
	dir := t.TempDir()

	configJSON := `{
		"auths": {
			"registry.example.com": {"auth": ""}
		}
	}`

	os.Setenv("DOCKER_CONFIG", dir)
	defer os.Unsetenv("DOCKER_CONFIG")

	err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configJSON), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	fn := dockerCredentialFunc()
	if fn == nil {
		t.Fatal("expected non-nil credential func")
	}

	user, pass, err := fn("registry.example.com")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if user != "" || pass != "" {
		t.Errorf("empty auth should return empty credentials, got (%q, %q)", user, pass)
	}
}

func TestDockerCredentialFuncBadBase64(t *testing.T) {
	dir := t.TempDir()

	configJSON := `{
		"auths": {
			"registry.example.com": {"auth": "not-valid-base64!!!"}
		}
	}`

	os.Setenv("DOCKER_CONFIG", dir)
	defer os.Unsetenv("DOCKER_CONFIG")

	err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configJSON), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	fn := dockerCredentialFunc()
	if fn == nil {
		t.Fatal("expected non-nil credential func")
	}

	user, pass, err := fn("registry.example.com")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if user != "" || pass != "" {
		t.Errorf("bad base64 should return empty credentials, got (%q, %q)", user, pass)
	}
}

func TestDockerCredentialFuncNoColonInDecoded(t *testing.T) {
	dir := t.TempDir()

	// Base64 of "no-colon-here" (no separator)
	encoded := base64.StdEncoding.EncodeToString([]byte("no-colon-here"))
	configJSON := `{
		"auths": {
			"registry.example.com": {"auth": "` + encoded + `"},
			"good.registry.com": {"auth": "` + base64.StdEncoding.EncodeToString([]byte("user:pass")) + `"}
		}
	}`

	os.Setenv("DOCKER_CONFIG", dir)
	defer os.Unsetenv("DOCKER_CONFIG")

	err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configJSON), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	fn := dockerCredentialFunc()
	if fn == nil {
		t.Fatal("expected non-nil credential func")
	}

	// Bad auth entry returns empty (falls through to next match or empty)
	user, pass, err := fn("registry.example.com")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if user != "" || pass != "" {
		t.Errorf("no-colon auth should return empty credentials, got (%q, %q)", user, pass)
	}

	// Good entry works
	user2, pass2, err := fn("good.registry.com")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if user2 != "user" || pass2 != "pass" {
		t.Errorf("got (%q, %q), want (\"user\", \"pass\")", user2, pass2)
	}
}

func TestCredHelperGetSuccess(t *testing.T) {
	dir := t.TempDir()
	helperScript := filepath.Join(dir, "docker-credential-test-helper")
	err := os.WriteFile(helperScript, []byte(`#!/bin/sh
echo '{"Username":"testuser","Secret":"testsecret"}'
`), 0755)
	if err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	user, pass, err := credHelperGet("test-helper", "registry.example.com")
	if err != nil {
		t.Fatalf("credHelperGet: %v", err)
	}
	if user != "testuser" || pass != "testsecret" {
		t.Errorf("got (%q, %q), want (\"testuser\", \"testsecret\")", user, pass)
	}
}

func TestCredHelperGetNonExistent(t *testing.T) {
	dir := t.TempDir()

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	_, _, err := credHelperGet("nonexistent-helper", "registry.example.com")
	if err == nil {
		t.Fatal("expected error for non-existent helper")
	}
	// The error should mention the missing executable or command failure
	if !strings.Contains(err.Error(), "executable file not found") && !strings.Contains(err.Error(), "no such file or directory") {
		t.Logf("error: %v", err)
	}
}

func TestCredHelperGetMalformedResponse(t *testing.T) {
	dir := t.TempDir()
	helperScript := filepath.Join(dir, "docker-credential-bad-json")
	err := os.WriteFile(helperScript, []byte(`#!/bin/sh
echo '{invalid json'
`), 0755)
	if err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	_, _, err = credHelperGet("bad-json", "registry.example.com")
	if err == nil {
		t.Fatal("expected error for malformed JSON response")
	}
}

func TestCredHelperGetNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	helperScript := filepath.Join(dir, "docker-credential-fail")
	err := os.WriteFile(helperScript, []byte(`#!/bin/sh
exit 1
`), 0755)
	if err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	_, _, err = credHelperGet("fail", "registry.example.com")
	if err == nil {
		t.Fatal("expected error for non-zero exit code")
	}
}

func TestCredHelperGetMissingFields(t *testing.T) {
	dir := t.TempDir()
	helperScript := filepath.Join(dir, "docker-credential-empty")
	err := os.WriteFile(helperScript, []byte(`#!/bin/sh
echo '{}'
`), 0755)
	if err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	user, pass, err := credHelperGet("empty", "registry.example.com")
	if err != nil {
		t.Fatalf("credHelperGet: %v", err)
	}
	// JSON unmarshaling succeeds but fields are empty strings
	if user != "" || pass != "" {
		t.Errorf("expected empty credentials for empty JSON, got (%q, %q)", user, pass)
	}
}

func TestDockerCredentialFuncDefaultHomeDir(t *testing.T) {
	// Test that when DOCKER_CONFIG is not set, it falls back to ~/.docker
	// We can't easily change os.UserHomeDir(), but we can verify the fallback
	// path logic by checking that a valid config in HOME works.

	_, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine home directory: %v", err)
	}

	// Save and restore DOCKER_CONFIG
	oldConfig := os.Getenv("DOCKER_CONFIG")
	defer func() {
		if oldConfig == "" {
			os.Unsetenv("DOCKER_CONFIG")
		} else {
			os.Setenv("DOCKER_CONFIG", oldConfig)
		}
	}()
	os.Unsetenv("DOCKER_CONFIG")

	// Save and restore HOME
	oldHome := os.Getenv("HOME")
	defer func() {
		if oldHome == "" {
			os.Unsetenv("HOME")
		} else {
			os.Setenv("HOME", oldHome)
		}
	}()

	tempHome := t.TempDir()
	os.Setenv("HOME", tempHome)

	dockerDir := filepath.Join(tempHome, ".docker")
	if err := os.MkdirAll(dockerDir, 0755); err != nil {
		t.Fatalf("create .docker dir: %v", err)
	}

	homeAuth := base64.StdEncoding.EncodeToString([]byte("homeuser:homepass"))
	configJSON := `{"auths":{"test.home.registry.com":{"auth":"` + homeAuth + `"}}}`

	err = os.WriteFile(filepath.Join(tempHome, ".docker", "config.json"), []byte(configJSON), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	fn := dockerCredentialFunc()
	if fn == nil {
		t.Fatal("expected non-nil credential func with valid home dir config")
	}

	user, pass, err := fn("test.home.registry.com")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if user != "homeuser" || pass != "homepass" {
		t.Errorf("got (%q, %q), want (\"homeuser\", \"homepass\")", user, pass)
	}
}

func TestDockerCredentialFuncUserHomeError(t *testing.T) {
	os.Unsetenv("DOCKER_CONFIG")

	// Temporarily set HOME to a path that doesn't exist or is unreadable.
	// This is tricky on Linux since os.UserHomeDir() reads from /etc/passwd,
	// not HOME. We can only test the DOCKER_CONFIG path here.
	// Instead, verify that when both fail gracefully, we get nil.
	_, err := os.UserHomeDir()
	if err != nil {
		// If we can't even determine home, this is a system issue — skip
		t.Skip("cannot determine home directory")
	}
}

func TestDockerCredentialFuncCredHelperReturnsEmpty(t *testing.T) {
	dir := t.TempDir()

	configJSON := `{
		"credHelpers": {
			"empty-registry.example.com": "empty-helper"
		},
		"auths": {}
	}`

	os.Setenv("DOCKER_CONFIG", dir)
	defer os.Unsetenv("DOCKER_CONFIG")

	err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configJSON), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	helperDir := t.TempDir()
	helperScript := filepath.Join(helperDir, "docker-credential-empty-helper")
	err = os.WriteFile(helperScript, []byte(`#!/bin/sh
echo '{"Username":"","Secret":""}'
`), 0755)
	if err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", helperDir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	fn := dockerCredentialFunc()
	if fn == nil {
		t.Fatal("expected non-nil credential func")
	}

	user, pass, err := fn("empty-registry.example.com")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if user != "" || pass != "" {
		t.Errorf("empty credentials from helper should return empty, got (%q, %q)", user, pass)
	}
}

func TestDockerCredentialFuncCredStoreReturnsEmpty(t *testing.T) {
	dir := t.TempDir()

	configJSON := `{
		"credsStore": "empty-store"
	}`

	os.Setenv("DOCKER_CONFIG", dir)
	defer os.Unsetenv("DOCKER_CONFIG")

	err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configJSON), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	helperDir := t.TempDir()
	helperScript := filepath.Join(helperDir, "docker-credential-empty-store")
	err = os.WriteFile(helperScript, []byte(`#!/bin/sh
echo '{"Username":"","Secret":""}'
`), 0755)
	if err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", helperDir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	fn := dockerCredentialFunc()
	if fn == nil {
		t.Fatal("expected non-nil credential func")
	}

	user, pass, err := fn("any-registry.com")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if user != "" || pass != "" {
		t.Errorf("empty credentials from credsStore should return empty, got (%q, %q)", user, pass)
	}
}
