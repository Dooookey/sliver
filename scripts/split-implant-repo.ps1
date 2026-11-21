# split-implant-repo.ps1
#
# Creates a standalone git repository for implant-cpp out of the sliver fork,
# keeping full commit history (git subtree split), so the C/C++ implant can live
# in its own GitHub repository while the fork keeps working as the reference.
#
# Usage:
#   pwsh -File scripts/split-implant-repo.ps1 -Target D:\GOPATH\src\C2\implant-cpp
#
# After it finishes:
#   1. create an empty repo on GitHub (no README, no licence)
#   2. git -C <Target> remote add origin git@github.com:<you>/implant-cpp.git
#   3. git -C <Target> push -u origin main
#
# To keep the fork and the new repo in sync afterwards, either
#   a) replace sliver/implant-cpp with a submodule:
#        git -C sliver rm -r --cached implant-cpp
#        git -C sliver submodule add <url> implant-cpp
#   or b) push from the fork with:  git subtree push --prefix=implant-cpp <remote> main

param(
  [string]$Target = (Join-Path (Split-Path -Parent $PSScriptRoot) '..\implant-cpp'),
  [string]$Branch = 'implant-cpp-standalone',
  [string]$RemoteBranch = 'main'
)

$ErrorActionPreference = 'Continue'   # native git writes progress to stderr

$repo = Split-Path -Parent $PSScriptRoot
if (-not (Test-Path (Join-Path $repo '.git'))) {
  throw "not inside the sliver git repository: $repo"
}
$prefix = Join-Path $repo 'implant-cpp'
if (-not (Test-Path $prefix)) {
  throw "implant-cpp not found under $repo"
}

Write-Host "==> splitting history for implant-cpp (branch $Branch)"
& git -C $repo branch -D $Branch *> $null
& git -C $repo subtree split --prefix=implant-cpp -b $Branch *> $null
if ($LASTEXITCODE -ne 0) { throw 'git subtree split failed' }

$Target = [System.IO.Path]::GetFullPath($Target)
if (Test-Path $Target) {
  Write-Host "==> target exists, refreshing refs: $Target"
  & git -C $Target fetch $repo $Branch
} else {
  Write-Host "==> creating standalone repo: $Target"
  New-Item -ItemType Directory -Force -Path $Target | Out-Null
  & git -C $Target init -q -b $RemoteBranch
  & git -C $Target fetch $repo $Branch
}
if ($LASTEXITCODE -ne 0) { throw 'fetch failed' }

& git -C $Target checkout -q -B $RemoteBranch FETCH_HEAD
if ($LASTEXITCODE -ne 0) { throw 'checkout failed' }

Write-Host ''
Write-Host '==> standalone repo ready'
& git -C $Target log --oneline -5
Write-Host ''
Write-Host 'Next steps (needs your GitHub account):'
Write-Host '  1) create an empty repo on GitHub, e.g. sliver-implant-cpp'
Write-Host "  2) git -C `"$Target`" remote add origin git@github.com:<you>/sliver-implant-cpp.git"
Write-Host "  3) git -C `"$Target`" push -u origin $RemoteBranch"
Write-Host ''
Write-Host 'Keep it wired to the fork (pick one):'
Write-Host '  a) submodule:  git -C sliver rm -r --cached implant-cpp; git -C sliver submodule add <url> implant-cpp'
Write-Host "  b) subtree:    git -C sliver subtree push --prefix=implant-cpp origin-cpp $RemoteBranch"
