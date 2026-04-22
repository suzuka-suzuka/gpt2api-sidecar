param(
  [string]$ConfigPath = "..\\config.yaml",
  [string]$GoVersion = "1.25.4"
)

$ErrorActionPreference = "Stop"

$Root = Resolve-Path (Join-Path $PSScriptRoot "..")
Push-Location $Root
try {
  $Config = Resolve-Path -ErrorAction SilentlyContinue (Join-Path $PSScriptRoot $ConfigPath)
  if (-not $Config) {
    $TargetConfig = Join-Path $Root "config.yaml"
    if (-not (Test-Path $TargetConfig)) {
      Copy-Item (Join-Path $Root "config.example.yaml") $TargetConfig
      Write-Host "Created config.yaml. Fill accounts[0].auth_token before starting."
      exit 1
    }
    $Config = Resolve-Path $TargetConfig
  }

  $ConfigText = Get-Content $Config -Raw
  if ($ConfigText -match '(?m)^\s*auth_token:\s*["'']?\s*["'']?\s*$') {
    Write-Host "config.yaml still has an empty auth_token. Fill accounts[0].auth_token first."
    exit 1
  }

  $ToolsDir = Join-Path $Root ".tools"
  $GoDir = Join-Path $ToolsDir "go"
  $GoExe = Join-Path $GoDir "bin\\go.exe"
  $ZipFile = Join-Path $ToolsDir "go$GoVersion.windows-amd64.zip"
  $DownloadUrl = "https://go.dev/dl/go$GoVersion.windows-amd64.zip"

  if (-not (Test-Path $GoExe)) {
    New-Item -ItemType Directory -Force -Path $ToolsDir | Out-Null
    if (-not (Test-Path $ZipFile)) {
      Write-Host "Downloading Go $GoVersion..."
      Invoke-WebRequest -Uri $DownloadUrl -OutFile $ZipFile
    }
    if (Test-Path $GoDir) {
      Remove-Item -Recurse -Force $GoDir
    }
    Write-Host "Extracting Go $GoVersion..."
    Expand-Archive -Path $ZipFile -DestinationPath $ToolsDir -Force
  }

  $env:GOROOT = $GoDir
  $env:PATH = "$GoDir\\bin;$env:PATH"

  & $GoExe version
  & $GoExe mod tidy
  & $GoExe run .\\cmd\\sidecar -config $Config
} finally {
  Pop-Location
}
