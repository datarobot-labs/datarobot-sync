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

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolate clears every variable Resolve reads, so a test never inherits the developer's own
// DataRobot credentials.
func isolate(t *testing.T) {
	t.Helper()

	for _, name := range []string{
		EnvEndpoint, EnvToken, EnvConfig, EnvCatalogID,
		EnvCLIDebug, EnvCLIVerbose, EnvCLISkipCertific, EnvCLICACert,
		EnvSSLCertFile, EnvPluginMode, "XDG_CONFIG_HOME", "HOME",
	} {
		t.Setenv(name, "")
	}

	// A home directory that exists but holds no config, so the default path is probed and
	// found absent rather than accidentally hitting the real one.
	t.Setenv("HOME", t.TempDir())
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "drconfig.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func TestResolveFromEnvironment(t *testing.T) {
	isolate(t)
	t.Setenv(EnvEndpoint, "https://app.datarobot.com/api/v2")
	t.Setenv(EnvToken, "token-from-env")
	t.Setenv(EnvCatalogID, "cat-from-env")

	resolved, err := Resolve(Overrides{})
	require.NoError(t, err)

	assert.Equal(t, "https://app.datarobot.com/api/v2", resolved.Endpoint)
	assert.Equal(t, "token-from-env", resolved.Token)
	assert.Equal(t, "cat-from-env", resolved.CatalogID)
}

func TestOverridesWinOverEnvironment(t *testing.T) {
	isolate(t)
	t.Setenv(EnvEndpoint, "https://from-env.example/api/v2")
	t.Setenv(EnvToken, "token")
	t.Setenv(EnvCatalogID, "from-env")

	resolved, err := Resolve(Overrides{
		Endpoint:  "https://from-flag.example/api/v2",
		CatalogID: "from-flag",
	})
	require.NoError(t, err)

	assert.Equal(t, "https://from-flag.example/api/v2", resolved.Endpoint)
	assert.Equal(t, "from-flag", resolved.CatalogID)
}

// Standalone, with no CLI to inject credentials, the config file has to be enough.
func TestResolveFallsBackToConfigFile(t *testing.T) {
	isolate(t)

	path := writeConfig(t, "endpoint: https://app.datarobot.com/api/v2\ntoken: token-from-file\n")

	resolved, err := Resolve(Overrides{ConfigFile: path})
	require.NoError(t, err)

	assert.Equal(t, "https://app.datarobot.com/api/v2", resolved.Endpoint)
	assert.Equal(t, "token-from-file", resolved.Token)
	assert.Equal(t, path, resolved.Source, "the source is reported so a bad token can be traced")
}

func TestConfigFileFoundViaEnvironment(t *testing.T) {
	isolate(t)

	path := writeConfig(t, "endpoint: https://x.example/api/v2\ntoken: t\n")
	t.Setenv(EnvConfig, path)

	resolved, err := Resolve(Overrides{})
	require.NoError(t, err)
	assert.Equal(t, "t", resolved.Token)
}

// A token in the environment with the endpoint only in the file, and vice versa: the two are
// resolved independently rather than as a pair.
func TestResolveMixesEnvironmentAndFile(t *testing.T) {
	isolate(t)

	path := writeConfig(t, "endpoint: https://from-file.example/api/v2\ntoken: from-file\n")
	t.Setenv(EnvToken, "from-env")

	resolved, err := Resolve(Overrides{ConfigFile: path})
	require.NoError(t, err)

	assert.Equal(t, "https://from-file.example/api/v2", resolved.Endpoint)
	assert.Equal(t, "from-env", resolved.Token)
}

// A user typing --endpoint reaches for the bare host, but every path the client builds is
// relative to the /api/v2 prefix.
func TestEndpointGainsAPIPrefix(t *testing.T) {
	isolate(t)
	t.Setenv(EnvToken, "t")

	cases := map[string]string{
		"https://app.datarobot.com":         "https://app.datarobot.com/api/v2",
		"https://app.datarobot.com/":        "https://app.datarobot.com/api/v2",
		"https://app.datarobot.com/api/v2":  "https://app.datarobot.com/api/v2",
		"https://app.datarobot.com/api/v2/": "https://app.datarobot.com/api/v2",
		"https://self.hosted.example/dr":    "https://self.hosted.example/dr/api/v2",
	}

	for given, want := range cases {
		resolved, err := Resolve(Overrides{Endpoint: given})
		require.NoError(t, err)
		assert.Equal(t, want, resolved.Endpoint, "endpoint %q", given)
	}
}

func TestResolveReportsMissingCredentials(t *testing.T) {
	isolate(t)

	_, err := Resolve(Overrides{})
	require.ErrorIs(t, err, ErrNoCredentials)
	require.ErrorContains(t, err, "DATAROBOT_API_TOKEN")
	require.ErrorContains(t, err, "dr auth login")
}

// Under the CLI these are injected automatically, so their absence points at the manifest
// rather than at the user.
func TestMissingCredentialsUnderPluginModeBlamesTheManifest(t *testing.T) {
	isolate(t)
	t.Setenv(EnvPluginMode, "1")

	_, err := Resolve(Overrides{})
	require.ErrorIs(t, err, ErrNoCredentials)
	require.ErrorContains(t, err, `"authentication": true`)
}

func TestTLSAndVerbosityForwardedFromCLI(t *testing.T) {
	isolate(t)
	t.Setenv(EnvToken, "t")
	t.Setenv(EnvEndpoint, "https://x.example/api/v2")
	t.Setenv(EnvCLISkipCertific, "1")
	t.Setenv(EnvCLIVerbose, "1")
	t.Setenv(EnvCLIDebug, "1")
	t.Setenv(EnvCLICACert, "/etc/ssl/dr.pem")

	resolved, err := Resolve(Overrides{})
	require.NoError(t, err)

	assert.True(t, resolved.InsecureSkipVerify)
	assert.True(t, resolved.Verbose)
	assert.True(t, resolved.Debug)
	assert.Equal(t, "/etc/ssl/dr.pem", resolved.CACertFile)
}

// Go honours SSL_CERT_FILE on Linux but not on macOS, so it is read explicitly.
func TestSSLCertFileIsReadExplicitly(t *testing.T) {
	isolate(t)
	t.Setenv(EnvToken, "t")
	t.Setenv(EnvEndpoint, "https://x.example/api/v2")
	t.Setenv(EnvSSLCertFile, "/etc/ssl/system.pem")

	resolved, err := Resolve(Overrides{})
	require.NoError(t, err)
	assert.Equal(t, "/etc/ssl/system.pem", resolved.CACertFile)

	// The CLI's own flag takes precedence over the conventional variable.
	t.Setenv(EnvCLICACert, "/etc/ssl/from-cli.pem")

	resolved, err = Resolve(Overrides{})
	require.NoError(t, err)
	assert.Equal(t, "/etc/ssl/from-cli.pem", resolved.CACertFile)
}

func TestInsecureOverrideCanTurnOffTheEnvironment(t *testing.T) {
	isolate(t)
	t.Setenv(EnvToken, "t")
	t.Setenv(EnvEndpoint, "https://x.example/api/v2")
	t.Setenv(EnvCLISkipCertific, "1")

	off := false

	resolved, err := Resolve(Overrides{InsecureSkipVerify: &off})
	require.NoError(t, err)
	assert.False(t, resolved.InsecureSkipVerify, "an explicit false must beat the inherited flag")
}

func TestUnparseableConfigFileIsReported(t *testing.T) {
	isolate(t)

	path := writeConfig(t, "endpoint: [unterminated\n")

	_, err := Resolve(Overrides{ConfigFile: path})
	require.ErrorContains(t, err, "parsing")
}

func TestMissingConfigFileIsNotAnError(t *testing.T) {
	isolate(t)
	t.Setenv(EnvEndpoint, "https://x.example/api/v2")
	t.Setenv(EnvToken, "t")

	resolved, err := Resolve(Overrides{ConfigFile: filepath.Join(t.TempDir(), "absent.yaml")})
	require.NoError(t, err)
	assert.Equal(t, "t", resolved.Token)
}

func TestFilePathPrecedence(t *testing.T) {
	isolate(t)

	explicit, err := FilePath("/explicit/path.yaml")
	require.NoError(t, err)
	assert.Equal(t, "/explicit/path.yaml", explicit)

	t.Setenv(EnvConfig, "/from/env.yaml")

	fromEnv, err := FilePath("")
	require.NoError(t, err)
	assert.Equal(t, "/from/env.yaml", fromEnv)

	t.Setenv(EnvConfig, "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")

	fromXDG, err := FilePath("")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/xdg", "datarobot", "drconfig.yaml"), fromXDG)
}

func TestRunningAsPlugin(t *testing.T) {
	isolate(t)
	assert.False(t, RunningAsPlugin())

	t.Setenv(EnvPluginMode, "1")
	assert.True(t, RunningAsPlugin())
}
