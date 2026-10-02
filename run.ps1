<#
.SYNOPSIS
  Compila e roda o lume.

.EXAMPLE
  .\run.ps1              # painel em http://127.0.0.1:6767, com arquivos pequenos para ver a árvore se mexer
  .\run.ps1 -Big         # tamanhos de produção (tabelas de 4 MB)
  .\run.ps1 -Sync        # fsync a cada escrita
  .\run.ps1 -Reset       # apaga os dados antes de iniciar
  .\run.ps1 -Dev         # servidor + Vite com hot reload em http://localhost:5183
  .\run.ps1 -Test        # testes do Go e build do frontend
  .\run.ps1 -Bench       # escritas, leituras e varredura por segundo
#>
param(
    [switch]$Big,
    [switch]$Sync,
    [switch]$Reset,
    [switch]$Dev,
    [switch]$Test,
    [switch]$Bench,
    [switch]$NoBrowser,
    [int]$Port = 6767
)

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
Set-Location $root

function Step($msg) { Write-Host "`n==> $msg" -ForegroundColor DarkYellow }
function Need($cmd, $hint) {
    if (-not (Get-Command $cmd -ErrorAction SilentlyContinue)) {
        Write-Host "Faltando: $cmd. $hint" -ForegroundColor Red
        exit 1
    }
}

Need go   'Instale Go 1.26+ em https://go.dev/dl/'
Need node 'Instale Node 22+ em https://nodejs.org/'

Push-Location ui
try {
    if (-not (Test-Path node_modules)) {
        Step 'Instalando dependências do frontend'
        npm install --no-audit --no-fund
        if ($LASTEXITCODE) { throw 'npm install falhou' }
    }
} finally { Pop-Location }

if ($Test) {
    Step 'Checagem de tipos e build do frontend'
    Push-Location ui; npm run build; $code = $LASTEXITCODE; Pop-Location
    if ($code) { exit $code }
    Step 'Testes do Go (inclui quedas simuladas e comparação com um modelo)'
    go vet ./...
    if ($LASTEXITCODE) { exit $LASTEXITCODE }
    go test -count=1 ./...
    exit $LASTEXITCODE
}

if ($Bench) {
    Step 'Benchmark com 500 mil chaves'
    go run ./cmd/lume bench
    exit $LASTEXITCODE
}

if ($Reset) {
    Step 'Apagando os dados (data/)'
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue (Join-Path $root 'data')
}

$serveArgs = @('-addr', "127.0.0.1:$Port", '-dir', (Join-Path $root 'data'))
if ($Big)  { $serveArgs += '-big' }
if ($Sync) { $serveArgs += '-sync' }

if ($Dev) {
    Step "Servidor em http://127.0.0.1:$Port (janela separada)"
    $backend = Start-Process -PassThru -FilePath go -ArgumentList (@('run', './cmd/lume') + $serveArgs) -WorkingDirectory $root
    try {
        Step 'Frontend com hot reload em http://localhost:5183'
        if (-not $NoBrowser) { Start-Process 'http://localhost:5183' }
        Push-Location ui
        npm run dev
    } finally {
        Pop-Location
        if ($backend -and -not $backend.HasExited) { taskkill /PID $backend.Id /T /F | Out-Null }
    }
    exit 0
}

Step 'Compilando o frontend'
Push-Location ui
try {
    npm run build
    if ($LASTEXITCODE) { throw 'build do frontend falhou' }
} finally { Pop-Location }

Step 'Compilando o binário (frontend embutido)'
New-Item -ItemType Directory -Force bin | Out-Null
go build -trimpath -ldflags '-s -w' -o bin/lume.exe ./cmd/lume
if ($LASTEXITCODE) { throw 'go build falhou' }

$url = "http://127.0.0.1:$Port"
Step "lume rodando em $url. Os arquivos do banco ficam em data\. Ctrl+C para parar."
if (-not $NoBrowser) {
    Start-Job -ScriptBlock {
        param($u)
        for ($i = 0; $i -lt 40; $i++) {
            try { Invoke-WebRequest "$u/api/view" -UseBasicParsing -TimeoutSec 1 | Out-Null; Start-Process $u; return } catch { Start-Sleep -Milliseconds 250 }
        }
    } -ArgumentList $url | Out-Null
}
& "$root\bin\lume.exe" @serveArgs
