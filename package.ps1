param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^\d+\.\d+\.\d+$')]
    [string]$Version,

    [Parameter(Mandatory = $true)]
    [ValidateSet('windows-amd64', 'linux-amd64', 'darwin-amd64', 'darwin-arm64')]
    [string]$Target,

    [string]$ReleaseDirectory = '',

    [string]$GoExecutable = 'go',

    [string]$NodeExecutable = 'node',

    [string]$GitExecutable = 'git'
)

$ErrorActionPreference = 'Stop'
$projectRoot = $PSScriptRoot
$goCommand = (Get-Command -Name $GoExecutable -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
$nodeCommand = (Get-Command -Name $NodeExecutable -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
$gitCommand = (Get-Command -Name $GitExecutable -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source

function Get-HostPlatform {
    if ($env:OS -eq 'Windows_NT') {
        return 'windows'
    }

    $systemName = (& uname -s).Trim()
    if ($LASTEXITCODE -ne 0) {
        throw 'Unable to determine the host platform.'
    }
    switch ($systemName) {
        'Darwin' { return 'darwin' }
        'Linux' { return 'linux' }
        default { throw "Unsupported host platform: $systemName" }
    }
}

function Assert-TargetBinary {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [string]$TargetName
    )

    $bytes = [IO.File]::ReadAllBytes($Path)
    switch ($TargetName) {
        'windows-amd64' {
            $peOffset = [BitConverter]::ToInt32($bytes, 0x3c)
            $signature = [BitConverter]::ToUInt32($bytes, $peOffset)
            $machine = [BitConverter]::ToUInt16($bytes, $peOffset + 4)
            $subsystem = [BitConverter]::ToUInt16($bytes, $peOffset + 92)
            if ($signature -ne 0x00004550 -or $machine -ne 0x8664 -or $subsystem -ne 2) {
                throw 'Invalid Windows AMD64 GUI executable.'
            }
        }
        'linux-amd64' {
            $machine = [BitConverter]::ToUInt16($bytes, 18)
            if ($bytes[0] -ne 0x7f -or $bytes[1] -ne 0x45 -or $bytes[2] -ne 0x4c -or
                $bytes[3] -ne 0x46 -or $bytes[4] -ne 2 -or $bytes[5] -ne 1 -or $machine -ne 62) {
                throw 'Invalid Linux AMD64 executable.'
            }
        }
        'darwin-amd64' {
            if ([BitConverter]::ToUInt32($bytes, 0) -ne 0xfeedfacf -or
                [BitConverter]::ToUInt32($bytes, 4) -ne 0x01000007) {
                throw 'Invalid macOS AMD64 executable.'
            }
        }
        'darwin-arm64' {
            if ([BitConverter]::ToUInt32($bytes, 0) -ne 0xfeedfacf -or
                [BitConverter]::ToUInt32($bytes, 4) -ne 0x0100000c) {
                throw 'Invalid macOS ARM64 executable.'
            }
        }
    }
}

$versionMatch = [regex]::Match(
    (Get-Content -Raw -LiteralPath (Join-Path $projectRoot 'main.go')),
    'applicationVersion\s*=\s*"([^"]+)"'
)
if (-not $versionMatch.Success -or $versionMatch.Groups[1].Value -ne $Version) {
    throw "Package version $Version does not match applicationVersion in main.go."
}

if ($ReleaseDirectory -eq '') {
    $ReleaseDirectory = Join-Path $projectRoot "dist\release-v$Version"
} elseif (-not [IO.Path]::IsPathRooted($ReleaseDirectory)) {
    $ReleaseDirectory = Join-Path $projectRoot $ReleaseDirectory
}
$ReleaseDirectory = [IO.Path]::GetFullPath($ReleaseDirectory)

$hostPlatform = Get-HostPlatform
$goOS, $goArch = $Target.Split('-', 2)
$cgoEnabled = '0'
$binaryName = 'codex-quota-router'
$installerSource = ''
$archiveExtension = '.tar.gz'
$linkerFlags = ''

switch ($Target) {
    'windows-amd64' {
        $binaryName = 'codex-quota-router.exe'
        $installerSource = 'windows\router.bat'
        $archiveExtension = '.zip'
        $linkerFlags = '-H=windowsgui'
    }
    'linux-amd64' {
        $installerSource = 'linux/router.sh'
    }
    'darwin-amd64' {
        $installerSource = 'macos/router.sh'
        $cgoEnabled = '1'
    }
    'darwin-arm64' {
        $installerSource = 'macos/router.sh'
        $cgoEnabled = '1'
    }
}

if ($goOS -eq 'darwin') {
    if ($hostPlatform -ne 'darwin') {
        throw 'macOS packages must be built on macOS with Xcode Command Line Tools and Cocoa SDK.'
    }
    $hostArchitecture = (& uname -m).Trim()
    $requiredArchitecture = if ($goArch -eq 'amd64') { 'x86_64' } else { 'arm64' }
    if ($hostArchitecture -ne $requiredArchitecture) {
        throw "Target $Target must be built and verified on a $requiredArchitecture Mac."
    }
}

$packageName = "codex-quota-router-$Target-v$Version"
$packageDirectory = [IO.Path]::GetFullPath((Join-Path $ReleaseDirectory $packageName))
$archivePath = [IO.Path]::GetFullPath((Join-Path $ReleaseDirectory "$packageName$archiveExtension"))
$releasePrefix = $ReleaseDirectory.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) +
    [IO.Path]::DirectorySeparatorChar
$pathComparison = if ($hostPlatform -eq 'windows') { [StringComparison]::OrdinalIgnoreCase } else { [StringComparison]::Ordinal }
if (-not $packageDirectory.StartsWith($releasePrefix, $pathComparison) -or
    -not $archivePath.StartsWith($releasePrefix, $pathComparison)) {
    throw 'Target output escaped the release directory.'
}
if ((Test-Path -LiteralPath $packageDirectory) -or (Test-Path -LiteralPath $archivePath)) {
    throw "Target output already exists: $packageName"
}

Push-Location $projectRoot
try {
    $gitStatus = @(& $gitCommand status --porcelain=v1 --untracked-files=all --ignore-submodules=none)
    if ($LASTEXITCODE -ne 0) {
        throw "git status failed with exit code $LASTEXITCODE."
    }
    if ($gitStatus.Count -ne 0) {
        throw 'Release packages must be created from a clean Git worktree.'
    }

    $requiredTag = "v$Version"
    $headTags = @(& $gitCommand tag --points-at HEAD | ForEach-Object { $_.Trim() })
    if ($LASTEXITCODE -ne 0) {
        throw "git tag failed with exit code $LASTEXITCODE."
    }
    if ($headTags -notcontains $requiredTag) {
        throw "HEAD must have the exact release tag $requiredTag."
    }

    & $nodeCommand --test (Join-Path $projectRoot 'web\tests\frontend.test.mjs')
    if ($LASTEXITCODE -ne 0) {
        throw "frontend tests failed with exit code $LASTEXITCODE."
    }

    $previousTestEnvironment = @{
        CGO_ENABLED = $env:CGO_ENABLED
        GOOS         = $env:GOOS
        GOARCH       = $env:GOARCH
    }
    try {
        $env:CGO_ENABLED = $null
        $env:GOOS = $null
        $env:GOARCH = $null
        & $goCommand test ./...
        if ($LASTEXITCODE -ne 0) {
            throw "go test failed with exit code $LASTEXITCODE."
        }
    } finally {
        $env:CGO_ENABLED = $previousTestEnvironment.CGO_ENABLED
        $env:GOOS = $previousTestEnvironment.GOOS
        $env:GOARCH = $previousTestEnvironment.GOARCH
    }
} finally {
    Pop-Location
}

try {
    New-Item -ItemType Directory -Path $packageDirectory | Out-Null
    $binaryPath = Join-Path $packageDirectory $binaryName

    $previousEnvironment = @{
        CGO_ENABLED = $env:CGO_ENABLED
        GOOS         = $env:GOOS
        GOARCH       = $env:GOARCH
    }

    Push-Location $projectRoot
    try {
        $env:CGO_ENABLED = $cgoEnabled
        $env:GOOS = $goOS
        $env:GOARCH = $goArch

        $buildArguments = @('build', '-trimpath')
        if ($linkerFlags -ne '') {
            $buildArguments += @('-ldflags', $linkerFlags)
        }
        $buildArguments += @('-o', $binaryPath, '.')
        & $goCommand @buildArguments
        if ($LASTEXITCODE -ne 0) {
            throw "go build failed with exit code $LASTEXITCODE."
        }
    } finally {
        $env:CGO_ENABLED = $previousEnvironment.CGO_ENABLED
        $env:GOOS = $previousEnvironment.GOOS
        $env:GOARCH = $previousEnvironment.GOARCH
        Pop-Location
    }

    Assert-TargetBinary -Path $binaryPath -TargetName $Target

    $installerName = [IO.Path]::GetFileName($installerSource)
    $installerPath = Join-Path $packageDirectory $installerName
    Copy-Item -LiteralPath (Join-Path $projectRoot $installerSource) -Destination $installerPath
    Copy-Item -LiteralPath (Join-Path $projectRoot 'README.md') -Destination $packageDirectory
    Copy-Item -LiteralPath (Join-Path $projectRoot 'THIRD_PARTY_NOTICES.md') -Destination $packageDirectory

    if ($Target -ne 'windows-amd64' -and $hostPlatform -ne 'windows') {
        & chmod 0755 $binaryPath $installerPath
        if ($LASTEXITCODE -ne 0) {
            throw 'Unable to set executable permissions.'
        }
    }

    if ($archiveExtension -eq '.zip') {
        Compress-Archive -LiteralPath $packageDirectory -DestinationPath $archivePath -CompressionLevel Optimal
    } else {
        $archiveArguments = @(
            'run', './tools/package-archive',
            $archivePath,
            $packageName,
            $binaryPath,
            $installerPath,
            (Join-Path $packageDirectory 'README.md'),
            (Join-Path $packageDirectory 'THIRD_PARTY_NOTICES.md')
        )
        & $goCommand @archiveArguments
        if ($LASTEXITCODE -ne 0) {
            throw "Archive creation failed with exit code $LASTEXITCODE."
        }
    }
} catch {
    if (Test-Path -LiteralPath $archivePath) {
        Remove-Item -LiteralPath $archivePath -Force
    }
    if (Test-Path -LiteralPath $packageDirectory) {
        Remove-Item -LiteralPath $packageDirectory -Recurse -Force
    }
    throw
}
