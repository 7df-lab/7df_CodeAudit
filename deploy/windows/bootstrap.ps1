<#
CodeAudit 生产态一键部署 Windows 引导（双壳：Git Bash 优先 → WSL2 兜底）
====================================================================
架构一句话：Docker Desktop 装在 Windows 侧（它是 Windows 应用，自带隐藏的
docker-desktop WSL 发行版承载 Linux 内核与 daemon）；本引导装的 Ubuntu（若走到
WSL 壳）只是 bash 部署脚本的运行环境——容器永远在 Docker Desktop 的 daemon 里，
与"直接在 Windows 用 Docker Desktop"是同一份引擎。选壳只影响"bash 在哪里跑"。

壳策略（2026-09-08）：
  1) 优先用 Windows 上已装的 Git Bash（Git for Windows 的 bash.exe）——仓库直接
     克隆在 NTFS，零额外发行版；
  2) 没有 Git Bash 时才走 WSL2 路径（启用 WSL → 装 Ubuntu → 仓库进 ext4）。

用法（建议管理员 PowerShell；Git Bash 壳不需要管理员）：
  powershell -ExecutionPolicy Bypass -File deploy\windows\bootstrap.ps1
  powershell ... -Action configure|deploy|status|stop|down
  powershell ... -RepoUrl <git-url> [-Dir <路径>] [-Distro Ubuntu-22.04]

前置：Windows 10 2004+/11；BIOS 虚拟化已开；Docker Desktop（缺则 winget 装）；
  Git Bash 壳额外需要 Python（缺则 winget 装，勿用商店占位 stub）。

Git Bash 壳的两道特有防线：
  - CRLF：克隆统一 `-c core.autocrlf=false`，克隆后校验 deploy 脚本无 \r，
    残留则 checkout-index 强制重检出为 LF（shell 脚本遇 CRLF 必炸）；
  - MSYS 工具面：bash 入口已内置 netstat/ipconfig 回退（无 ss/ip 也可跑）；
    python3 缺失时若存在 python 则自动建 ~/bin/python3 垫片(shim)；
    unzip 缺失仅告警（仅影响"素材缺失需全量拉取"的分支）。

访问：Windows 本机浏览器 http://localhost:<控制台口/网关口>；局域网其它设备
  运行 deploy\windows\expose-lan.ps1（netsh portproxy）或 Win11 镜像网络。
#>
#Requires -Version 5.1
param(
    [ValidateSet("configure","deploy","status","stop","down")]
    [string]$Action = "deploy",
    [string]$RepoUrl = "",
    [string]$Dir = "",              # Git Bash 壳=Windows 路径；WSL 壳=WSL 内路径（~/ 开头）
    [string]$Distro = "Ubuntu-22.04"
)
$ErrorActionPreference = "Stop"
function Say($m){ Write-Host "[win-deploy] $m" }
function Die($m){ Write-Host "[win-deploy] ERROR: $m" -ForegroundColor Red; exit 1 }
function To-PosixPath([string]$p){
    $p = $p.TrimEnd('\')
    if ($p -match '^([A-Za-z]):[\\/](.*)$') { return '/' + $Matches[1].ToLower() + '/' + ($Matches[2] -replace '\\','/') }
    return ($p -replace '\\','/')
}

# ---- [1/3] Docker Desktop（Windows 侧，两壳共用同一 daemon）--------------------
Say "== [1/3] Docker Desktop（Windows 侧）=="
$dockerCli = Get-Command docker -ErrorAction SilentlyContinue
if (-not $dockerCli) {
    Say "未检测到 docker CLI —— 经 winget 安装 Docker Desktop..."
    winget install -e --id Docker.DockerDesktop --accept-source-agreements --accept-package-agreements
    if ($LASTEXITCODE -ne 0) { Die "Docker Desktop 安装失败，请手工安装后重跑。" }
    Die "请启动 Docker Desktop（桌面图标，等右下角鲸鱼图标稳定），然后重跑本脚本。"
}
docker version --format '{{.Server.Version}}' 2>&1 | Out-Null
if ($LASTEXITCODE -ne 0) {
    Die "docker daemon 不可达 —— 请启动 Docker Desktop（桌面图标，等鲸鱼图标稳定）后重跑。"
}
Say "docker daemon OK（$(& docker version --format '{{.Server.Version}}' 2>$null)）"

# ---- [2/3] 选壳：Git Bash 优先，WSL 兜底 --------------------------------------
Say "== [2/3] 选壳 =="
$gitBash = $null
$gitExe = (Get-Command git -ErrorAction SilentlyContinue).Source
if ($gitExe -and (Test-Path $gitExe)) {
    $cand = $gitExe -replace '\\cmd\\git\.exe$', '\bin\bash.exe'
    if (Test-Path $cand) { $gitBash = $cand }
}
if (-not $gitBash) {
    foreach ($c in @("$env:ProgramFiles\Git\bin\bash.exe",
                     "${env:ProgramFiles(x86)}\Git\bin\bash.exe",
                     "$env:LOCALAPPDATA\Programs\Git\bin\bash.exe")) {
        if ($c -and (Test-Path $c)) { $gitBash = $c; break }
    }
}

if ($gitBash) {
    # ================= Git Bash 壳（仓库在 NTFS，无需管理员/WSL）=================
    Say "命中 Git Bash：$gitBash"
    if (-not $gitExe) { $gitExe = $gitBash -replace '\\bin\\bash\.exe$', '\cmd\git.exe' }
    if (-not $Dir) { $Dir = Join-Path $env:USERPROFILE "codeaudit-umbrella" }
    $posix = Convert-To-PosixPath $Dir

    if (-not (Test-Path (Join-Path $Dir ".git"))) {
        if (-not $RepoUrl) { Die "首次使用请提供 -RepoUrl <伞仓 git 地址>。" }
        Say "克隆（NTFS，core.autocrlf=false 防 CRLF）..."
        & $gitExe clone -c core.autocrlf=false --recurse-submodules $RepoUrl $Dir
        if ($LASTEXITCODE -ne 0) { Die "克隆失败（检查 -RepoUrl 与网络）。" }
    }
    & $gitExe -C $Dir -c core.autocrlf=false submodule update --init --recursive | Out-Null

    # CRLF 防线：脚本面必须 LF；残留则强制重检出
    $crlf = (& $gitBash -lc "cd '$posix' && grep -c `"`$(printf '\r')`" deploy/production-deploy.sh 2>/dev/null || true")
    if ("$crlf".Trim() -ne "" -and "$crlf".Trim() -ne "0") {
        Say "检出含 CRLF —— 归一化为 LF（autocrlf=false + checkout-index 强制重检出）..."
        & $gitExe -C $Dir config core.autocrlf false
        & $gitExe -C $Dir submodule foreach --recursive "git config core.autocrlf false; git checkout-index -a -f" | Out-Null
        & $gitExe -C $Dir checkout-index -a -f
        $crlf2 = (& $gitBash -lc "cd '$posix' && grep -c `"`$(printf '\r')`" deploy/production-deploy.sh 2>/dev/null || true")
        if ("$crlf2".Trim() -ne "" -and "$crlf2".Trim() -ne "0") { Die "CRLF 归一化失败，请手工重克隆（-c core.autocrlf=false）。" }
        Say "CRLF 已归一化"
    }

    # python3（Git Bash 常缺别名；仅有 python 时建 ~/bin/python3 垫片(shim)）
    & $gitBash -lc "command -v python3 >/dev/null 2>&1" ; $hasPy3 = ($LASTEXITCODE -eq 0)
    & $gitBash -lc "command -v python >/dev/null 2>&1"  ; $hasPy  = ($LASTEXITCODE -eq 0)
    if (-not $hasPy3 -and -not $hasPy) {
        Say "缺 Python —— winget install Python.Python.3.12（装完请重开终端重跑）..."
        winget install -e --id Python.Python.3.12 --accept-source-agreements --accept-package-agreements
        Die "Python 已安装：请重开 PowerShell/Git Bash 后重跑本脚本。"
    }
    if (-not $hasPy3 -and $hasPy) {
        & $gitBash -lc 'mkdir -p ~/bin && printf ''#!/bin/sh\nexec python "$@"\n'' > ~/bin/python3 && chmod +x ~/bin/python3'
        Say "已建 ~/bin/python3 垫片(指向 python)"
    }
    & $gitBash -lc "command -v unzip >/dev/null 2>&1" ; if ($LASTEXITCODE -ne 0) {
        Say "△ Git Bash 缺 unzip：仅影响'沙箱素材缺失需全量拉取'的分支（在位即零下载不受影响）；可装 unzip 或改用 WSL 壳。"
    }

    Say "== [3/3] Git Bash 壳执行（$Action）=="
    & $gitBash -lc "cd '$posix' && bash deploy/production-deploy.sh $Action"
    if ($LASTEXITCODE -ne 0) { Die "部署动作 '$Action' 失败（输出见上）。" }
}
else {
    # ================= WSL2 壳（兜底）===========================================
    Say "未发现 Git Bash —— 走 WSL2 路径。"
    $principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        Die "WSL 路径需要管理员（启用 WSL 功能）；或先安装 Git for Windows 后重跑（Git Bash 壳免管理员）。"
    }
    $winVer = [System.Environment]::OSVersion.Version
    if ($winVer.Build -lt 19041) { Die "需要 Windows 10 2004(build 19041)+ 或 Windows 11，当前 build=$($winVer.Build)。" }

    Say "启用 WSL2 + 发行版 $Distro..."
    wsl --status 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Say "WSL 未安装 —— wsl --install --no-distribution（完成后通常需要重启 Windows，然后重跑本脚本）"
        wsl --install --no-distribution
        if ($LASTEXITCODE -ne 0) { Die "WSL 安装失败（确认 BIOS 虚拟化已开启）。" }
        Die "WSL 功能已启用：请重启 Windows 后重跑本脚本。"
    }
    $distroList = ((wsl -l -q | Out-String) -replace "`0", "") -split "\r?\n" | ForEach-Object { $_.Trim() } | Where-Object { $_ -ne "" }
    if (-not ($distroList -contains $Distro)) {
        Say "安装发行版 $Distro（首次启动需设置 UNIX 用户名/口令）..."
        wsl --install -d $Distro
        if ($LASTEXITCODE -ne 0) { Die "发行版安装失败。可用 `wsl -l -o` 查看列表后用 -Distro 指定。" }
    }
    # Docker Desktop WSL 集成：发行版内 docker 必须可用
    wsl -d $Distro -- bash -lc "docker version --format '{{.Server.Version}}'" 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Die "WSL 发行版 $Distro 内 docker 不可用 —— 打开 Docker Desktop → Settings → Resources → WSL Integration 勾选 $Distro → Apply & Restart，然后重跑本脚本。"
    }
    if (-not $Dir) { $Dir = "~/codeaudit-umbrella" }
    if ($RepoUrl) {
        wsl -d $Distro -u root -- bash -lc "command -v git >/dev/null || { apt-get update && apt-get install -y git; }"
        wsl -d $Distro -- bash -lc "test -d $Dir/.git || git clone --recurse-submodules $RepoUrl $Dir"
        if ($LASTEXITCODE -ne 0) { Die "仓库克隆失败（检查 -RepoUrl 与网络）。" }
    } else {
        wsl -d $Distro -- bash -lc "test -d $Dir/.git" 2>&1 | Out-Null
        if ($LASTEXITCODE -ne 0) { Die "WSL 内未发现仓库 $Dir —— 首次使用请提供 -RepoUrl。" }
        Say "使用 WSL 内已存在的 $Dir"
    }
    Say "== [3/3] WSL 壳执行（$Action）=="
    wsl -d $Distro -- bash -lc "cd $Dir && bash deploy/production-deploy.sh $Action"
    if ($LASTEXITCODE -ne 0) { Die "部署动作 '$Action' 失败（输出见上）。" }
}

Say ""
Say "完成。Windows 本机浏览器访问 http://localhost:<控制台口/网关口>（deploy 完成横幅打印实际端口）。"
Say "局域网其它设备访问需端口转发：管理员运行 deploy\windows\expose-lan.ps1（-Remove 撤销）。"
