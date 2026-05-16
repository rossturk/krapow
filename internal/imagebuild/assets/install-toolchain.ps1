# Installs the krapow universal Windows toolchain into the base image during
# bake (post-OS-install, pre-sysprep). Goal: parity with GitHub-hosted
# windows-latest so workflows written for `runs-on: windows-latest` Just Work.
#
# Coverage:
#   Tier 0 — Compiler base:
#       VS 2022 Build Tools (C++ + ARM64 cross), Chocolatey, Git for Windows,
#       LLVM. GNU /usr/bin/link.exe removed to prevent collision with MSVC link.
#   Tier 1 — CLI utilities workflows commonly assume:
#       jq, gh, git-lfs, cmake, ninja, 7zip, openssl, zstd, aria2, WiX, NSIS,
#       InnoSetup, VSWhere, Bazelisk, MSYS2 (junctioned to C:\msys64).
#   Tier 2 — System language runtimes on PATH:
#       Node LTS, Python (latest 3.x), Go (latest), Ruby (latest).
#   Tier 3 — Hosted tool-cache pre-populated for actions/setup-*:
#       Python 3.10-3.14, Node 20/22/24, Go 1.22-1.25, Ruby 3.2-4.0,
#       Temurin JDK 8/11/17/21/25 at C:\hostedtoolcache\windows\...
#   Tier 4 — Cloud + container CLIs:
#       AWS CLI, Azure CLI, gcloud, kubectl, helm, docker CLI.
#
# Idempotent: every step checks existing state and skips if already done.
# Safe to re-run.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

# ===================================================================
# Helpers
# ===================================================================

function Invoke-DownloadWithRetry {
    param([string]$Uri, [string]$OutFile, [int]$MaxAttempts = 3)
    for ($i = 1; $i -le $MaxAttempts; $i++) {
        try {
            Invoke-WebRequest -Uri $Uri -OutFile $OutFile -UseBasicParsing -TimeoutSec 900
            return
        } catch {
            if ($i -eq $MaxAttempts) { throw }
            Write-Host "  download attempt $i failed ($($_.Exception.Message)); retrying in $(5 * $i)s"
            Start-Sleep -Seconds (5 * $i)
        }
    }
}

function Install-ChocoPackage {
    param([string]$Name, [string[]]$ExtraArgs = @())
    $listed = & choco list --exact $Name --limit-output 2>$null
    if ($listed -and $listed -match "^$([Regex]::Escape($Name))\|") {
        Write-Host "[choco] $Name — already installed"
        return
    }
    Write-Host "[choco] installing $Name"
    $cmdArgs = @('install', '-y', '--no-progress', $Name) + $ExtraArgs
    & choco @cmdArgs | Out-Host
    if ($LASTEXITCODE -ne 0) { throw "choco install $Name failed: exit $LASTEXITCODE" }
}

# ===================================================================
# Tier 0 — Compiler base
# ===================================================================

function Install-Tier0-Base {
    # ----- Visual Studio 2022 Build Tools (~6 GB, ~10 min) -----
    if (-not (Test-Path 'C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools')) {
        Write-Host "Installing Visual Studio 2022 Build Tools..."
        $url = 'https://aka.ms/vs/17/release/vs_BuildTools.exe'
        $dst = "$env:TEMP\vs_BuildTools.exe"
        Invoke-DownloadWithRetry -Uri $url -OutFile $dst
        $vsArgs = @(
            '--quiet','--wait','--norestart','--nocache'
            '--add','Microsoft.VisualStudio.Workload.VCTools'
            '--add','Microsoft.VisualStudio.Component.VC.Tools.x86.x64'
            '--add','Microsoft.VisualStudio.Component.VC.Tools.ARM64'
            '--add','Microsoft.VisualStudio.Component.Windows11SDK.22621'
        )
        $vsOut = "$env:TEMP\vs-install.out.log"
        $vsErr = "$env:TEMP\vs-install.err.log"
        $p = Start-Process -FilePath $dst -ArgumentList $vsArgs -Wait -PassThru -NoNewWindow `
            -RedirectStandardOutput $vsOut -RedirectStandardError $vsErr
        if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) {
            $tail = if (Test-Path $vsOut) { Get-Content $vsOut -Tail 20 -ErrorAction SilentlyContinue } else { "(no log)" }
            throw "VS Build Tools install failed: exit $($p.ExitCode). Tail:`n$tail"
        }
        Write-Host "VS Build Tools installed"
    }

    # ----- Chocolatey -----
    if (-not (Get-Command choco -ErrorAction SilentlyContinue)) {
        Write-Host "Installing Chocolatey..."
        Set-ExecutionPolicy Bypass -Scope Process -Force
        [System.Net.ServicePointManager]::SecurityProtocol = `
            [System.Net.ServicePointManager]::SecurityProtocol -bor 3072
        Invoke-Expression ((New-Object System.Net.WebClient).DownloadString(
            'https://community.chocolatey.org/install.ps1'))
        $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
        if ($machinePath -notlike '*chocolatey\bin*') {
            [Environment]::SetEnvironmentVariable('Path',
                "$machinePath;C:\ProgramData\chocolatey\bin", 'Machine')
        }
    }
    # Refresh this session's PATH so the just-installed choco is callable below.
    $env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' +
                [Environment]::GetEnvironmentVariable('Path', 'User')

    # ----- Git for Windows -----
    Install-ChocoPackage 'git'
    # GNU coreutils `link` at C:\Program Files\Git\usr\bin\link.exe collides
    # with MSVC's link.exe in Git Bash regardless of PATH order (MSYS resolves
    # /usr/bin/* ahead of Windows PATH for many invocations). The GNU variant
    # is a thin wrapper over `ln` that almost nobody invokes directly.
    Remove-Item 'C:\Program Files\Git\usr\bin\link.exe' -Force -ErrorAction SilentlyContinue

    # ----- LLVM (clang, clang-cl, lld) -----
    Install-ChocoPackage 'llvm'
}

# ===================================================================
# Tier 1 — CLI utilities
# ===================================================================

function Install-Tier1-Utilities {
    $packages = @(
        'jq',           # JSON CLI
        'gh',           # GitHub CLI
        'git-lfs',      # Large File Storage
        'cmake',        # build system
        'ninja',        # build system
        '7zip',         # archives
        'openssl',      # TLS CLI + libs
        'zstandard',    # zstd compressor
        'aria2',        # downloader
        'wixtoolset',   # MSI authoring
        'nsis',         # installer authoring
        'innosetup',    # installer authoring
        'vswhere',      # locate VS installations
        'bazelisk'      # bazel version launcher
    )
    foreach ($pkg in $packages) { Install-ChocoPackage $pkg }

    # ----- MSYS2 -----
    # choco's msys2 package installs to C:\tools\msys64; GH hosted runners ship
    # at C:\msys64. Junction so scripts hardcoding either path work, and
    # msys2/setup-msys2 sees a pre-existing install (skips its bootstrap).
    Install-ChocoPackage 'msys2'
    if (-not (Test-Path 'C:\msys64')) {
        if (Test-Path 'C:\tools\msys64') {
            New-Item -ItemType Junction -Path 'C:\msys64' -Target 'C:\tools\msys64' | Out-Null
            Write-Host "junctioned C:\msys64 -> C:\tools\msys64"
        }
    }
}

# ===================================================================
# Tier 2 — System language runtimes
# ===================================================================
#
# These provide the "default" interpreter present at the start of every
# workflow, so shell steps like `python --version` work without setup-python.
# Tier 3 additionally seeds the hosted tool-cache so setup-* actions don't
# re-download every run.

function Install-Tier2-Languages {
    Install-ChocoPackage 'nodejs-lts'   # Node 20 (current LTS)
    Install-ChocoPackage 'python'       # Python 3.x latest
    Install-ChocoPackage 'golang'       # Go latest
    Install-ChocoPackage 'ruby'         # Ruby latest (RubyInstaller2)
}

# ===================================================================
# Tier 3 — Hosted tool-cache (actions/setup-* parity)
# ===================================================================
#
# Layout from @actions/tool-cache:
#   C:\hostedtoolcache\windows\<Tool>\<version>\<arch>\          install dir
#   C:\hostedtoolcache\windows\<Tool>\<version>\<arch>.complete  marker file
#
# setup-python / setup-node / setup-go / setup-java check the marker and skip
# the download entirely when present.
#
# Versions below match what GH's windows-2022 image currently ships; bump as
# the upstream image rolls forward. Per-version failures are non-fatal — we
# log and continue so one bad version doesn't kill the whole bake.

$script:ToolCacheRoot = 'C:\hostedtoolcache\windows'

function Install-CachedTool {
    param(
        [string]$Tool,
        [string]$Version,
        [string]$Arch,
        [string]$Url,
        [ValidateSet('zip','tar.gz','7z')] [string]$ArchiveType,
        [switch]$PythonLayout  # archive has setup.ps1 + tools/; use tools/* as source
    )
    $installDir = Join-Path $script:ToolCacheRoot "$Tool\$Version\$Arch"
    $marker = "$installDir.complete"
    if (Test-Path $marker) {
        Write-Host "[tool-cache] $Tool $Version $Arch — already cached"
        return
    }
    Write-Host "[tool-cache] $Tool $Version $Arch — fetching"
    $tmp = Join-Path $env:TEMP "tc-$Tool-$Version-$(Get-Random)"
    if (Test-Path $tmp) { Remove-Item $tmp -Recurse -Force }
    New-Item -ItemType Directory -Path $tmp -Force | Out-Null
    $archive = Join-Path $tmp "archive.$ArchiveType"
    Invoke-DownloadWithRetry -Uri $Url -OutFile $archive

    $extractDir = Join-Path $tmp 'extract'
    New-Item -ItemType Directory -Path $extractDir -Force | Out-Null
    switch ($ArchiveType) {
        'zip' {
            Expand-Archive -Path $archive -DestinationPath $extractDir -Force
        }
        'tar.gz' {
            tar -xzf $archive -C $extractDir
            if ($LASTEXITCODE -ne 0) { throw "tar extract failed: $archive" }
        }
        '7z' {
            $sevenZip = (Get-Command 7z -ErrorAction SilentlyContinue).Source
            if (-not $sevenZip) { $sevenZip = 'C:\Program Files\7-Zip\7z.exe' }
            & $sevenZip x -y "-o$extractDir" $archive | Out-Null
            if ($LASTEXITCODE -ne 0) { throw "7z extract failed: $archive" }
        }
    }
    Remove-Item $archive -Force

    # Find the install source: auto-detect single top-level dir, then optionally
    # descend into `tools/` for python-versions archives.
    $source = $extractDir
    $top = Get-ChildItem $extractDir
    if ($top.Count -eq 1 -and $top[0].PSIsContainer) { $source = $top[0].FullName }
    if ($PythonLayout) {
        $tools = Join-Path $source 'tools'
        if (Test-Path $tools) { $source = $tools }
    }

    if (Test-Path $installDir) { Remove-Item $installDir -Recurse -Force }
    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    Get-ChildItem $source -Force | Move-Item -Destination $installDir -Force
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue

    New-Item -ItemType File -Path $marker -Force | Out-Null
    Write-Host "[tool-cache] $Tool $Version $Arch — installed"
}

function Install-Tier3-ToolCache {
    New-Item -ItemType Directory -Path $script:ToolCacheRoot -Force | Out-Null

    # ----- Python (actions/python-versions) -----
    $pythonVersions = @('3.10.11','3.11.9','3.12.10','3.13.13','3.14.4')
    try {
        $pyManifest = Invoke-RestMethod -UseBasicParsing `
            -Uri 'https://raw.githubusercontent.com/actions/python-versions/main/versions-manifest.json'
        foreach ($v in $pythonVersions) {
            try {
                $entry = $pyManifest | Where-Object { $_.version -eq $v } | Select-Object -First 1
                if (-not $entry) { Write-Host "[tool-cache] Python $v not in manifest, skipping"; continue }
                $file = $entry.files | Where-Object { $_.platform -eq 'win32' -and $_.arch -eq 'x64' } | Select-Object -First 1
                if (-not $file) { Write-Host "[tool-cache] Python $v win32/x64 not in manifest, skipping"; continue }
                Install-CachedTool -Tool 'Python' -Version $v -Arch 'x64' `
                    -Url $file.download_url -ArchiveType 'zip' -PythonLayout
            } catch { Write-Host "[tool-cache] Python $v failed: $($_.Exception.Message)" }
        }
    } catch { Write-Host "[tool-cache] could not fetch python-versions manifest: $($_.Exception.Message)" }

    # ----- Node (actions/node-versions) -----
    $nodeVersions = @('20.20.2','22.22.2','24.15.0')
    try {
        $nodeManifest = Invoke-RestMethod -UseBasicParsing `
            -Uri 'https://raw.githubusercontent.com/actions/node-versions/main/versions-manifest.json'
        foreach ($v in $nodeVersions) {
            try {
                $entry = $nodeManifest | Where-Object { $_.version -eq $v } | Select-Object -First 1
                if (-not $entry) { Write-Host "[tool-cache] Node $v not in manifest, skipping"; continue }
                $file = $entry.files | Where-Object { $_.platform -eq 'win32' -and $_.arch -eq 'x64' } | Select-Object -First 1
                if (-not $file) { Write-Host "[tool-cache] Node $v win32/x64 not in manifest, skipping"; continue }
                Install-CachedTool -Tool 'node' -Version $v -Arch 'x64' `
                    -Url $file.download_url -ArchiveType 'zip'
            } catch { Write-Host "[tool-cache] Node $v failed: $($_.Exception.Message)" }
        }
    } catch { Write-Host "[tool-cache] could not fetch node-versions manifest: $($_.Exception.Message)" }

    # ----- Go (actions/go-versions) -----
    $goVersions = @('1.22.12','1.23.12','1.24.13','1.25.10')
    try {
        $goManifest = Invoke-RestMethod -UseBasicParsing `
            -Uri 'https://raw.githubusercontent.com/actions/go-versions/main/versions-manifest.json'
        foreach ($v in $goVersions) {
            try {
                $entry = $goManifest | Where-Object { $_.version -eq $v } | Select-Object -First 1
                if (-not $entry) { Write-Host "[tool-cache] Go $v not in manifest, skipping"; continue }
                $file = $entry.files | Where-Object { $_.platform -eq 'win32' -and $_.arch -eq 'x64' } | Select-Object -First 1
                if (-not $file) { Write-Host "[tool-cache] Go $v win32/x64 not in manifest, skipping"; continue }
                Install-CachedTool -Tool 'go' -Version $v -Arch 'x64' `
                    -Url $file.download_url -ArchiveType 'zip'
            } catch { Write-Host "[tool-cache] Go $v failed: $($_.Exception.Message)" }
        }
    } catch { Write-Host "[tool-cache] could not fetch go-versions manifest: $($_.Exception.Message)" }

    # ----- Ruby (RubyInstaller2 direct) -----
    # ruby/setup-ruby on Windows pulls these same RubyInstaller2 .7z archives.
    $rubyVersions = @('3.2.11','3.3.11','3.4.9','4.0.3')
    foreach ($v in $rubyVersions) {
        try {
            $url = "https://github.com/oneclick/rubyinstaller2/releases/download/RubyInstaller-$v-1/rubyinstaller-$v-1-x64.7z"
            Install-CachedTool -Tool 'Ruby' -Version $v -Arch 'x64' `
                -Url $url -ArchiveType '7z'
        } catch { Write-Host "[tool-cache] Ruby $v failed: $($_.Exception.Message)" }
    }

    # ----- Java (Adoptium Temurin) -----
    # setup-java tool-cache key: Java_Temurin-Hotspot_jdk\<semver>\x64
    # Adoptium API redirects to the binary; Invoke-WebRequest follows redirects.
    $javaReleases = @(
        @{ semver = '8.0.482'; release = 'jdk8u482-b08' },
        @{ semver = '11.0.31'; release = 'jdk-11.0.31%2B11' },
        @{ semver = '17.0.19'; release = 'jdk-17.0.19%2B10' },
        @{ semver = '21.0.11'; release = 'jdk-21.0.11%2B10' },
        @{ semver = '25.0.3';  release = 'jdk-25.0.3%2B9' }
    )
    foreach ($j in $javaReleases) {
        try {
            $url = "https://api.adoptium.net/v3/binary/version/$($j.release)/windows/x64/jdk/hotspot/normal/eclipse?project=jdk"
            Install-CachedTool -Tool 'Java_Temurin-Hotspot_jdk' -Version $j.semver -Arch 'x64' `
                -Url $url -ArchiveType 'zip'
        } catch { Write-Host "[tool-cache] Java $($j.semver) failed: $($_.Exception.Message)" }
    }
}

# ===================================================================
# Tier 4 — Cloud and container tooling
# ===================================================================
#
# Note on Docker: GH hosted ships the Docker engine; we only bake the CLI.
# Running the engine on Windows requires Hyper-V + container feature config
# that's brittle in a baked image and rarely needed for typical CI use cases.

function Install-Tier4-Cloud {
    $packages = @(
        'awscli',          # AWS CLI v2
        'azure-cli',       # Azure CLI
        'gcloudsdk',       # Google Cloud SDK
        'kubernetes-cli',  # kubectl
        'kubernetes-helm', # helm
        'docker-cli'       # docker.exe client only (no engine)
    )
    foreach ($pkg in $packages) { Install-ChocoPackage $pkg }
}

# ===================================================================
# Machine PATH normalization (belt-and-suspenders)
# ===================================================================
#
# Installers write to HKLM\...\Environment\Path. Registry writes are lazy on
# Windows; if we power-off the bake VM before they're flushed (force-stop,
# BSOD), the image captures binaries on disk without the PATH update.
# Explicitly normalize PATH so even an unflushed installer doesn't bite us.

function Set-MachinePath {
    # Static paths that always apply.
    $wantOnPath = @(
        'C:\ProgramData\chocolatey\bin',
        'C:\Program Files\Git\cmd',
        'C:\Program Files\Git\bin',
        'C:\Program Files\LLVM\bin',
        'C:\Program Files\CMake\bin',
        'C:\Program Files\nodejs',
        'C:\Program Files\Go\bin',
        'C:\Program Files\Amazon\AWSCLIV2',
        'C:\Program Files (x86)\Microsoft SDKs\Azure\CLI2\wbin'
    )
    # Dynamic paths: choco installs these under versioned directory names.
    $pythonHome = Get-ChildItem 'C:\' -Directory -Filter 'Python3*' -ErrorAction SilentlyContinue |
                  Sort-Object Name -Descending | Select-Object -First 1
    if ($pythonHome) {
        $wantOnPath += $pythonHome.FullName
        $wantOnPath += (Join-Path $pythonHome.FullName 'Scripts')
    }
    $rubyHome = Get-ChildItem 'C:\tools' -Directory -Filter 'ruby*' -ErrorAction SilentlyContinue |
                Sort-Object Name -Descending | Select-Object -First 1
    if ($rubyHome) {
        $wantOnPath += (Join-Path $rubyHome.FullName 'bin')
    }

    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    foreach ($p in $wantOnPath) {
        if ($machinePath -notlike "*$p*") {
            $machinePath = "$machinePath;$p"
            Write-Host "added to machine PATH: $p"
        }
    }
    [Environment]::SetEnvironmentVariable('Path', $machinePath, 'Machine')
}

# ===================================================================
# Main
# ===================================================================

Install-Tier0-Base
Install-Tier1-Utilities
Install-Tier2-Languages
Install-Tier3-ToolCache
Install-Tier4-Cloud
Set-MachinePath

Write-Host "krapow toolchain installed"
