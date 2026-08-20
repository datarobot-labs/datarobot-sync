# Wrapper shipped inside the `dr` plugin archive. The archive carries one binary per
# platform, so this picks the right one and runs it with the arguments untouched.
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

$path = Join-Path $scriptDir $binary

& $path @args
exit $LASTEXITCODE
