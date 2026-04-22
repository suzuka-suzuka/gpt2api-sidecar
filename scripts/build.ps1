param(
  [string]$GoVersion = "1.25.4"
)

$ErrorActionPreference = "Stop"
$Root = Resolve-Path (Join-Path $PSScriptRoot "..")
Push-Location $Root
try {
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

  New-Item -ItemType Directory -Force -Path (Join-Path $Root "bin") | Out-Null
  & $GoExe version
  & $GoExe mod tidy
  & $GoExe build -o .\\bin\\gpt2api-sidecar.exe .\\cmd\\sidecar
} finally {
  Pop-Location
}
