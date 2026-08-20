// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

// Package config resolves how to reach DataRobot and which container to sync.
//
// dr-sync runs both as a `dr` CLI plugin and standalone. Under the CLI it inherits
// DATAROBOT_ENDPOINT and DATAROBOT_API_TOKEN, already validated, because its plugin manifest
// declares authentication. Standalone it has to find the same values itself, which is why the
// config file is read as a fallback rather than only trusted from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Environment variables dr-sync reads.
const (
	// EnvEndpoint and EnvToken are set by the `dr` CLI for a plugin with
	// "authentication": true.
	EnvEndpoint = "DATAROBOT_ENDPOINT"
	// EnvToken names the variable, and holds no secret itself.
	EnvToken = "DATAROBOT_API_TOKEN" //nolint:gosec // this is a variable name, not a credential
	// EnvConfig points at the active config file, also set by the CLI.
	EnvConfig = "DATAROBOT_CONFIG"
	// EnvCatalogID names the container to sync, so a CI job can set it once.
	EnvCatalogID = "DR_SYNC_CATALOG_ID"

	// The CLI forwards its universal flags as environment variables when they appear before
	// the plugin name.
	EnvCLIDebug        = "DATAROBOT_CLI_DEBUG"
	EnvCLIVerbose      = "DATAROBOT_CLI_VERBOSE"
	EnvCLISkipCertific = "DATAROBOT_CLI_SKIP_CERTIFICATE_CHECK"
	EnvCLICACert       = "DATAROBOT_CLI_CA_CERT"
	// EnvSSLCertFile is the conventional CA override. Go honours it on Linux but not on
	// macOS, so it is read explicitly rather than relied on.
	EnvSSLCertFile = "SSL_CERT_FILE"
	// EnvPluginMode tells us the `dr` CLI launched this process.
	EnvPluginMode = "DR_PLUGIN_MODE"
)

// Resolved is the effective configuration for a run.
type Resolved struct {
	Endpoint  string
	Token     string
	CatalogID string
	// Source records where the credentials came from, for the diagnostic when they are wrong.
	Source string
	// CACertFile and InsecureSkipVerify mirror the CLI's TLS flags.
	CACertFile         string
	InsecureSkipVerify bool
	Verbose            bool
	Debug              bool
}

// Overrides are the values a caller supplied explicitly, which win over everything else.
type Overrides struct {
	Endpoint   string
	CatalogID  string
	ConfigFile string
	CACertFile string
	// InsecureSkipVerify is a pointer so "not specified" is distinguishable from "false".
	InsecureSkipVerify *bool
}

// file is the subset of drconfig.yaml dr-sync cares about.
type file struct {
	Endpoint string `yaml:"endpoint"`
	Token    string `yaml:"token"`
}

// ErrNoCredentials means neither the environment nor the config file supplied what is needed.
var ErrNoCredentials = errors.New("no DataRobot credentials found")

// Resolve determines the endpoint, token and container for this run.
//
// The token is deliberately not accepted as a command-line flag anywhere: a flag would put it
// in the process table and the shell history.
func Resolve(overrides Overrides) (*Resolved, error) {
	resolved := &Resolved{
		Endpoint:           strings.TrimSpace(overrides.Endpoint),
		Token:              strings.TrimSpace(os.Getenv(EnvToken)),
		CatalogID:          strings.TrimSpace(overrides.CatalogID),
		CACertFile:         overrides.CACertFile,
		InsecureSkipVerify: os.Getenv(EnvCLISkipCertific) == "1",
		Verbose:            os.Getenv(EnvCLIVerbose) == "1",
		Debug:              os.Getenv(EnvCLIDebug) == "1",
		Source:             "environment",
	}

	if resolved.Endpoint == "" {
		resolved.Endpoint = strings.TrimSpace(os.Getenv(EnvEndpoint))
	}

	if resolved.CatalogID == "" {
		resolved.CatalogID = strings.TrimSpace(os.Getenv(EnvCatalogID))
	}

	if overrides.InsecureSkipVerify != nil {
		resolved.InsecureSkipVerify = *overrides.InsecureSkipVerify
	}

	if resolved.CACertFile == "" {
		resolved.CACertFile = firstNonEmpty(os.Getenv(EnvCLICACert), os.Getenv(EnvSSLCertFile))
	}

	if resolved.Endpoint != "" && resolved.Token != "" {
		resolved.Endpoint = normalizeEndpoint(resolved.Endpoint)

		return resolved, nil
	}

	if err := resolved.fillFromFile(overrides.ConfigFile); err != nil {
		return nil, err
	}

	if resolved.Endpoint == "" || resolved.Token == "" {
		return nil, credentialsError(overrides.ConfigFile)
	}

	resolved.Endpoint = normalizeEndpoint(resolved.Endpoint)

	return resolved, nil
}

// fillFromFile reads the missing pieces out of drconfig.yaml.
func (r *Resolved) fillFromFile(explicitPath string) error {
	path, err := FilePath(explicitPath)
	if err != nil {
		return err
	}

	if path == "" {
		return nil
	}

	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	var parsed file
	if err := yaml.Unmarshal(content, &parsed); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}

	if r.Endpoint == "" {
		r.Endpoint = strings.TrimSpace(parsed.Endpoint)
	}

	if r.Token == "" {
		r.Token = strings.TrimSpace(parsed.Token)
		r.Source = path
	}

	return nil
}

// FilePath returns the config file to read: the explicit path, then $DATAROBOT_CONFIG,
// then the CLI's own default location.
func FilePath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	if fromEnv := os.Getenv(EnvConfig); fromEnv != "" {
		return fromEnv, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		// No home directory is not fatal on its own: the environment may still carry
		// everything needed.
		return "", nil
	}

	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "datarobot", "drconfig.yaml"), nil
	}

	return filepath.Join(home, ".config", "datarobot", "drconfig.yaml"), nil
}

// normalizeEndpoint makes sure the endpoint carries the API prefix.
//
// The `dr` CLI stores it with /api/v2 already appended, but a user typing --endpoint by hand
// will reach for the bare host, and every path this client builds is relative to the prefix.
func normalizeEndpoint(endpoint string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(endpoint), "/")

	if strings.HasSuffix(trimmed, "/api/v2") {
		return trimmed
	}

	return trimmed + "/api/v2"
}

// credentialsError explains what is missing and how to supply it.
func credentialsError(explicitConfig string) error {
	// A failure here only costs the hint its file path, so the error text degrades rather
	// than the error being replaced by a worse one.
	path, pathErr := FilePath(explicitConfig)
	if pathErr != nil {
		path = ""
	}

	var hint strings.Builder

	hint.WriteString("set DATAROBOT_ENDPOINT and DATAROBOT_API_TOKEN")

	if path != "" {
		fmt.Fprintf(&hint, ", or run `dr auth login` to populate %s", path)
	}

	if os.Getenv(EnvPluginMode) == "1" {
		// Under the CLI these are injected automatically, so their absence points at the
		// plugin manifest rather than at the user.
		hint.WriteString(" (running as a dr plugin, so this usually means the plugin manifest is missing \"authentication\": true)")
	}

	return fmt.Errorf("%w: %s", ErrNoCredentials, hint.String())
}

// RunningAsPlugin reports whether the `dr` CLI launched this process.
func RunningAsPlugin() bool {
	return os.Getenv(EnvPluginMode) == "1"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
