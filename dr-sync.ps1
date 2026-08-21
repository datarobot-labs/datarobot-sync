# Wrapper shipped inside the `dr` plugin archive. The archive carries one binary per
# platform, so this picks the right one and runs it with the arguments untouched.
#
# The binaries live under bin/, not next to this script: the CLI's plugin-script scanner
# treats any extension-less dr-prefixed file in the plugin root as a candidate script, so a
# flat layout lets it pick a wrong-platform binary directly instead of this wrapper.
$ErrorActionPreference = 'Stop'

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path

$binary = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'dr-sync-windows-amd64.exe' }
    'ARM64' { 'dr-sync-windows-arm64.exe' }
    default {
        Write-Error "dr-sync: unsupported architecture: $env:PROCESSOR_ARCHITECTURE"
        exit 1
    }
}

$path = Join-Path (Join-Path $scriptDir 'bin') $binary

& $path @args
exit $LASTEXITCODE
