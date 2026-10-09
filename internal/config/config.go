// Copyright 2026 DataRobot, Inc. and its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package config resolves how to reach DataRobot and which container to sync.
//
// dr-sync runs both as a `dr` CLI plugin and standalone. Under the CLI it inherits
// DATAROBOT_ENDPOINT and DATAROBOT_API_TOKEN, already validated, because its plugin manifest
// declares authentication. Standalone it has to find the same values itself, which is why
// drconfig.yaml is read as a fallback rather than the environment being the only source.
//
// Precedence is viper's: an explicit flag beats an environment variable, which beats the
// config file. The variable names do not share a prefix — DATAROBOT_* for credentials,
// DR_SYNC_* for dr-sync's own settings — so each one is bound explicitly rather than through
// AutomaticEnv.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
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

// Viper keys for the values that come from more than one place.
const (
	KeyEndpoint   = "endpoint"
	KeyToken      = "token"
	KeyCatalogID  = "catalog-id"
	KeyCACert     = "ca-cert"
	KeyInsecure   = "insecure-skip-verify"
	KeyConfigFile = "config"
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
//
// Kept as an explicit struct rather than reading a global viper: it keeps the resolution
// testable, and it makes the precedence visible at the call site.
type Overrides struct {
	Endpoint   string
	CatalogID  string
	ConfigFile string
	CACertFile string
	// InsecureSkipVerify is a pointer so "not specified" is distinguishable from "false".
	InsecureSkipVerify *bool
}

// ErrNoCredentials means neither the environment nor the config file supplied what is needed.
var ErrNoCredentials = errors.New("no DataRobot credentials found")

// newViper builds an isolated viper carrying the environment bindings and, when it exists, the
// config file.
//
// A fresh instance rather than the package-level singleton, so two resolutions in one process
// — tests, most obviously — cannot contaminate each other.
func newViper(configFile string) (*viper.Viper, string, error) {
	v := viper.New()

	// Explicit bindings, because the variables do not share a prefix.
	bindings := map[string]string{
		KeyEndpoint:  EnvEndpoint,
		KeyToken:     EnvToken,
		KeyCatalogID: EnvCatalogID,
	}

	for key, variable := range bindings {
		if err := v.BindEnv(key, variable); err != nil {
			return nil, "", fmt.Errorf("binding %s: %w", variable, err)
		}
	}

	path, err := FilePath(configFile)
	if err != nil {
		return nil, "", err
	}

	if path == "" {
		return v, "", nil
	}

	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	if err := v.ReadInConfig(); err != nil {
		// A missing file is the normal case when everything comes from the environment.
		var missing *os.PathError
		if errors.As(err, &missing) || os.IsNotExist(err) {
			return v, "", nil
		}

		return nil, "", fmt.Errorf("parsing %s: %w", path, err)
	}

	return v, path, nil
}

// Resolve determines the endpoint, token and container for this run.
//
// The token is deliberately not accepted as a command-line flag anywhere: a flag would put it
// in the process table and the shell history.
func Resolve(overrides Overrides) (*Resolved, error) {
	v, configPath, err := newViper(overrides.ConfigFile)
	if err != nil {
		return nil, err
	}

	// Explicit flags outrank everything viper knows about.
	if overrides.Endpoint != "" {
		v.Set(KeyEndpoint, overrides.Endpoint)
	}

	if overrides.CatalogID != "" {
		v.Set(KeyCatalogID, overrides.CatalogID)
	}

	resolved := &Resolved{
		Endpoint:   strings.TrimSpace(v.GetString(KeyEndpoint)),
		Token:      strings.TrimSpace(v.GetString(KeyToken)),
		CatalogID:  strings.TrimSpace(v.GetString(KeyCatalogID)),
		CACertFile: overrides.CACertFile,
		Source:     "environment",
	}

	applyForwardedFlags(resolved)

	// The token is the one value worth tracing, because a stale one in a config file is a
	// confusing failure.
	if configPath != "" && os.Getenv(EnvToken) == "" && resolved.Token != "" {
		resolved.Source = configPath
	}

	if overrides.InsecureSkipVerify != nil {
		resolved.InsecureSkipVerify = *overrides.InsecureSkipVerify
	}

	if resolved.Endpoint == "" || resolved.Token == "" {
		return nil, credentialsError(overrides.ConfigFile)
	}

	resolved.Endpoint = normalizeEndpoint(resolved.Endpoint)

	return resolved, nil
}

// applyForwardedFlags reads the settings the `dr` CLI passes down as environment variables
// when its universal flags appear before the plugin name.
func applyForwardedFlags(resolved *Resolved) {
	resolved.InsecureSkipVerify = os.Getenv(EnvCLISkipCertific) == "1"
	resolved.Verbose = os.Getenv(EnvCLIVerbose) == "1"
	resolved.Debug = os.Getenv(EnvCLIDebug) == "1"

	if resolved.CACertFile == "" {
		resolved.CACertFile = firstNonEmpty(os.Getenv(EnvCLICACert), os.Getenv(EnvSSLCertFile))
	}
}

// FilePath returns the config file to read: the explicit path, then $DATAROBOT_CONFIG, then
// the CLI's own default location.
func FilePath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	if fromEnv := os.Getenv(EnvConfig); fromEnv != "" {
		return fromEnv, nil
	}

	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "datarobot", "drconfig.yaml"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		// No home directory is not fatal on its own: the environment may still carry
		// everything needed.
		return "", nil
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

	if RunningAsPlugin() {
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

// BindFlags lets a flag set contribute to resolution, at the top of the precedence order.
//
// This is what makes drconfig.yaml able to carry dr-sync's own settings — a `catalog-id:` key,
// say — without every command growing a special case: viper reconciles flag, environment and
// file, and a flag the user did not type does not override the file.
func BindFlags(flags *pflag.FlagSet) (*Binding, error) {
	v, path, err := newViper(flagString(flags, KeyConfigFile))
	if err != nil {
		return nil, err
	}

	for _, key := range []string{KeyEndpoint, KeyCatalogID, KeyCACert, KeyInsecure} {
		flag := flags.Lookup(key)
		if flag == nil {
			continue
		}

		if err := v.BindPFlag(key, flag); err != nil {
			return nil, fmt.Errorf("binding --%s: %w", key, err)
		}
	}

	return &Binding{viper: v, configPath: path, flags: flags}, nil
}

// Binding is a resolved view over flags, environment and config file.
type Binding struct {
	viper      *viper.Viper
	configPath string
	flags      *pflag.FlagSet
}

// Resolve produces the effective configuration.
func (b *Binding) Resolve() (*Resolved, error) {
	resolved := &Resolved{
		Endpoint:   strings.TrimSpace(b.viper.GetString(KeyEndpoint)),
		Token:      strings.TrimSpace(b.viper.GetString(KeyToken)),
		CatalogID:  strings.TrimSpace(b.viper.GetString(KeyCatalogID)),
		CACertFile: strings.TrimSpace(b.viper.GetString(KeyCACert)),
		Source:     "environment",
	}

	insecureFromFlag := b.viper.GetBool(KeyInsecure)

	applyForwardedFlags(resolved)

	// The dr CLI forwards -k as an environment variable; an explicitly typed flag still wins.
	if b.changed(KeyInsecure) {
		resolved.InsecureSkipVerify = insecureFromFlag
	}

	if b.configPath != "" && os.Getenv(EnvToken) == "" && resolved.Token != "" {
		resolved.Source = b.configPath
	}

	if resolved.Endpoint == "" || resolved.Token == "" {
		return nil, credentialsError(flagString(b.flags, KeyConfigFile))
	}

	resolved.Endpoint = normalizeEndpoint(resolved.Endpoint)

	return resolved, nil
}

// changed reports whether the user actually typed a flag, as opposed to it holding its default.
func (b *Binding) changed(name string) bool {
	flag := b.flags.Lookup(name)

	return flag != nil && flag.Changed
}

func flagString(flags *pflag.FlagSet, name string) string {
	if flags == nil {
		return ""
	}

	flag := flags.Lookup(name)
	if flag == nil {
		return ""
	}

	return flag.Value.String()
}
