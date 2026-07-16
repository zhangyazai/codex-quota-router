$ErrorActionPreference = 'Stop'
$packageScript = Join-Path (Split-Path -Parent $PSScriptRoot) 'package.ps1'
& $packageScript -Target 'windows-amd64' @args
