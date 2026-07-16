param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^\d+\.\d+\.\d+$')]
    [string]$Version,

    [Parameter(Mandatory = $true)]
    [ValidateSet('windows-amd64', 'linux-amd64', 'darwin-amd64', 'darwin-arm64')]
    [string[]]$Targets,

    [string]$ReleaseDirectory = ''
)

$ErrorActionPreference = 'Stop'
$projectRoot = $PSScriptRoot
if ($ReleaseDirectory -eq '') {
    $ReleaseDirectory = Join-Path $projectRoot "dist\release-v$Version"
} elseif (-not [IO.Path]::IsPathRooted($ReleaseDirectory)) {
    $ReleaseDirectory = Join-Path $projectRoot $ReleaseDirectory
}
$ReleaseDirectory = [IO.Path]::GetFullPath($ReleaseDirectory)

$archives = foreach ($target in ($Targets | Sort-Object -Unique)) {
    $extension = if ($target -eq 'windows-amd64') { '.zip' } else { '.tar.gz' }
    $name = "codex-quota-router-$target-v$Version$extension"
    $path = Join-Path $ReleaseDirectory $name
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Missing release archive: $name"
    }
    Get-Item -LiteralPath $path
}

$checksumLines = $archives |
    Sort-Object Name |
    ForEach-Object {
        $hash = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
        "$hash  $($_.Name)"
    }
$checksumPath = Join-Path $ReleaseDirectory 'SHA256SUMS.txt'
$temporaryPath = Join-Path $ReleaseDirectory ('.SHA256SUMS.' + [guid]::NewGuid().ToString('N') + '.tmp')
try {
    [IO.File]::WriteAllText($temporaryPath, ($checksumLines -join "`n") + "`n", [Text.Encoding]::ASCII)
    Move-Item -LiteralPath $temporaryPath -Destination $checksumPath -Force
} finally {
    if (Test-Path -LiteralPath $temporaryPath) {
        Remove-Item -LiteralPath $temporaryPath -Force
    }
}
