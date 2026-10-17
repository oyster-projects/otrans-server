[CmdletBinding()]
param(
  [ValidateSet('x86', 'arm')]
  [string]$Platform = 'x86',
  [ValidateRange(1024, 65535)]
  [int]$ServicePort = 18990,
  [string]$FnpackPath = '',
  [switch]$SkipFnpack
)

$ErrorActionPreference = 'Stop'
$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$stageRoot = [System.IO.Path]::GetFullPath((Join-Path $repoRoot "dist\fnos-$Platform"))
$distRoot = [System.IO.Path]::GetFullPath((Join-Path $repoRoot 'dist'))

if (-not $stageRoot.StartsWith($distRoot + [System.IO.Path]::DirectorySeparatorChar, [System.StringComparison]::OrdinalIgnoreCase)) {
  throw "Refusing to replace a staging path outside $distRoot"
}
if (Test-Path -LiteralPath $stageRoot) {
  Remove-Item -LiteralPath $stageRoot -Recurse -Force
}

Copy-Item -LiteralPath (Join-Path $repoRoot 'packaging\fnos\template') -Destination $stageRoot -Recurse
$manifestPath = Join-Path $stageRoot 'manifest'
$manifest = Get-Content -LiteralPath $manifestPath -Raw
$manifest = $manifest.Replace('@@PLATFORM@@', $Platform).Replace('@@SERVICE_PORT@@', $ServicePort.ToString())
Set-Content -LiteralPath $manifestPath -Value $manifest -NoNewline
$uiConfigPath = Join-Path $stageRoot 'app\ui\config'
$uiConfig = Get-Content -LiteralPath $uiConfigPath -Raw
Set-Content -LiteralPath $uiConfigPath -Value ($uiConfig.Replace('@@SERVICE_PORT@@', $ServicePort.ToString())) -NoNewline

$goArch = if ($Platform -eq 'x86') { 'amd64' } else { 'arm64' }
$binaryPath = Join-Path $stageRoot 'app\otransd'
New-Item -ItemType Directory -Path (Split-Path $binaryPath) -Force | Out-Null
$env:GOOS = 'linux'
$env:GOARCH = $goArch
$env:CGO_ENABLED = '0'
& go build -trimpath -ldflags '-s -w' -o $binaryPath ./cmd/otransd
if ($LASTEXITCODE -ne 0) {
  throw "Go build failed for linux/$goArch"
}

function New-OTransIcon([string]$Path, [int]$Size) {
  Add-Type -AssemblyName System.Drawing
  $bitmap = [System.Drawing.Bitmap]::new($Size, $Size)
  $graphics = [System.Drawing.Graphics]::FromImage($bitmap)
  try {
    $graphics.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
    $background = [System.Drawing.SolidBrush]::new([System.Drawing.Color]::FromArgb(68, 102, 238))
    $foreground = [System.Drawing.Pen]::new([System.Drawing.Color]::White, [Math]::Max(3, $Size * 0.085))
    try {
      $graphics.FillRectangle($background, 0, 0, $Size, $Size)
      $foreground.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
      $foreground.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
      $graphics.DrawLine($foreground, $Size * 0.22, $Size * 0.38, $Size * 0.76, $Size * 0.38)
      $graphics.DrawLine($foreground, $Size * 0.63, $Size * 0.25, $Size * 0.76, $Size * 0.38)
      $graphics.DrawLine($foreground, $Size * 0.63, $Size * 0.51, $Size * 0.76, $Size * 0.38)
      $graphics.DrawLine($foreground, $Size * 0.78, $Size * 0.64, $Size * 0.24, $Size * 0.64)
      $graphics.DrawLine($foreground, $Size * 0.37, $Size * 0.51, $Size * 0.24, $Size * 0.64)
      $graphics.DrawLine($foreground, $Size * 0.37, $Size * 0.77, $Size * 0.24, $Size * 0.64)
    } finally {
      $foreground.Dispose()
      $background.Dispose()
    }
    $bitmap.Save($Path, [System.Drawing.Imaging.ImageFormat]::Png)
  } finally {
    $graphics.Dispose()
    $bitmap.Dispose()
  }
}

New-OTransIcon -Path (Join-Path $stageRoot 'ICON.PNG') -Size 64
New-OTransIcon -Path (Join-Path $stageRoot 'ICON_256.PNG') -Size 256
$uiImagesPath = Join-Path $stageRoot 'app\ui\images'
New-Item -ItemType Directory -Path $uiImagesPath -Force | Out-Null
New-OTransIcon -Path (Join-Path $uiImagesPath 'icon_64.png') -Size 64
New-OTransIcon -Path (Join-Path $uiImagesPath 'icon_256.png') -Size 256

if ($SkipFnpack) {
  Write-Host "Staged fnOS package at $stageRoot"
  exit 0
}

$resolvedFnpack = ''
if ($FnpackPath.Length -gt 0) {
  $resolvedFnpack = [System.IO.Path]::GetFullPath($FnpackPath)
  if (-not (Test-Path -LiteralPath $resolvedFnpack -PathType Leaf)) {
    throw "fnpack does not exist: $resolvedFnpack"
  }
} else {
  $fnpack = Get-Command fnpack -ErrorAction SilentlyContinue
  if ($null -eq $fnpack) {
    throw 'fnpack is not installed or not on PATH. Pass -FnpackPath, or rerun with -SkipFnpack.'
  }
  $resolvedFnpack = $fnpack.Source
}

Push-Location $stageRoot
try {
  & $resolvedFnpack build
  if ($LASTEXITCODE -ne 0) {
    throw 'fnpack build failed'
  }
  $outputFpk = Join-Path $stageRoot 'OTrans.fpk'
  if (-not (Test-Path -LiteralPath $outputFpk -PathType Leaf)) {
    throw 'fnpack did not produce OTrans.fpk; inspect the packing output above.'
  }
  Write-Host "Built $outputFpk"
} finally {
  Pop-Location
}
