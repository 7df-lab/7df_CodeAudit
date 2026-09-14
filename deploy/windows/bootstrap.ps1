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

用法（建议管理员 PowerShell；若 Docker Desktop 已在运行，Git Bash 壳免管理员——
  缺 Docker 时 winget 安装需要管理员/交互 UAC）：
  powershell -ExecutionPolicy Bypass -File deploy\windows\bootstrap.ps1
  powershell ... -Action configure|deploy|status|stop|down
  powershell ... -RepoUrl <git-url> [-Dir <路径>] [-Distro Ubuntu-22.04]

前置：Windows 10 2004+/11；BIOS 虚拟化已开；Docker Desktop（缺则 winget 装，
  winget 亦缺时给手工指引）；Git Bash 壳额外需要 Python（缺则 winget 装；
  商店占位 stub 以"可真实执行"为判据，不认 command -v 命中）。

两壳共有的三道防线：
  - CRLF：克隆统一 -c core.autocrlf=false；autocrlf=false 无条件落盘伞仓+子仓
    （防后续 git 操作按全局配置 re-smudge）；哨兵 deploy/windows/crlf_check.sh
    全量扫描伞仓+子仓的跟踪 .sh/Dockerfile*，命中才走修复——修复守卫内建于
    crlf_check.sh：与行尾无关的未提交改动退出 3 拒修（防丢数据），纯行尾脏
    （re-smudge 形态）自动归一且不丢内容；
  - 脚本传输（PS5.1 根治）：PS5.1 原生传参对内嵌双引号不转义（7.3 才修），
    bash.exe 按 MSVCRT 规则剥引号/切参——bash -lc 脚本串经命令行到达即碎
    （探测恒假/守卫碎裂/空格路径 cd 断）。全部 bash 脚本改经临时文件（Git Bash
    壳）或 stdin→WSL 内固定路径（WSL 壳）下发，脚本内容不经命令行；
  - 参数与工具面：所有值经环境变量（CA_REPO_DIR/CA_ACTION/WSLENV）下发，bash 侧
    "$VAR" 引用，无 shell 插值注入面，路径含空格/单引号安全；bash 入口已内置
    netstat/ipconfig 回退；python3 缺失且有 python 时自动建 ~/bin/python3 垫片
    (shim)；unzip 缺失仅告警（只影响素材全量拉取分支）。

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
# PS5.1 管道到原生命令默认 ASCII——WSL 壳脚本经 stdin 下发，显式抬到 UTF-8 无 BOM
$OutputEncoding = New-Object System.Text.UTF8Encoding($false)
function Say($m){ Write-Host "[win-deploy] $m" }
function Die($m){ Write-Host "[win-deploy] ERROR: $m" -ForegroundColor Red; exit 1 }
function Probe {
    # PS5.1 陷阱：$ErrorActionPreference=Stop 时原生命令的 stderr 一经
    # 2>&1/2>$null 合流即变终止性 NativeCommandError——"daemon 未启动/WSL 未装/
    # 集成未勾"这些最需要友好提示的探测恰好全是 stderr 输出，裸写会让脚本在 Die
    # 之前崩出堆栈。探测统一经此：局部降回 Continue 吞掉错误流，成败只看 $LASTEXITCODE。
    $eap = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { & $args 2>&1 | Out-Null } finally { $ErrorActionPreference = $eap }
    return ($LASTEXITCODE -eq 0)
}
function To-PosixPath([string]$p){
    $p = $p.TrimEnd('\')
    if ($p -match '^[A-Za-z]:$') { Die "不要把仓库放在盘根 '$p'——请用子目录（如 C:\Users\me\codeaudit-umbrella）。" }
    if ($p -match '^[A-Za-z]:[^\\/]') { Die "不支持盘符相对路径 '$p'——请用完整路径（如 C:\Users\me\codeaudit-umbrella）。" }
    if ($p -match '^([A-Za-z]):[\\/](.*)$') { return '/' + $Matches[1].ToLower() + '/' + ($Matches[2] -replace '\\','/') }
    return ($p -replace '\\','/')
}
function Invoke-GitBashSh {
    # PS5.1→bash 脚本传输：PS5.1 原生传参对内嵌双引号不转义
    # （7.3 经 PSNativeCommandArgumentPassing 才修），bash.exe 按 MSVCRT 规则把内嵌
    # 引号当定界符剥除、在落出引用态的空格处切断 argv——任何内嵌引号的 bash -lc 串
    # 到达即碎（python 探测恒假死循环/守卫脚本语法错空转/含空格路径 cd 断）。
    # 改为：脚本串落临时 .sh 文件（UTF-8 无 BOM、LF，内容不经命令行），argv 只传
    # 文件路径；参数值仍走环境变量。-l 保持原 -lc 登录壳语义（~/bin 进 PATH，
    # python3 垫片可见）。$LASTEXITCODE = bash 退出码，调用方沿用原判错模式。
    param([string]$BashExe, [string]$ScriptText)
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("ca_" + [Guid]::NewGuid().ToString('N') + ".sh")
    [IO.File]::WriteAllText($tmp, ($ScriptText -replace "`r", "") + "`n", (New-Object System.Text.UTF8Encoding($false)))
    try { & $BashExe -l $tmp }
    finally { Remove-Item $tmp -ErrorAction SilentlyContinue }
}
function Probe-GitBashSh {
    # 同 Probe 的语义（EAP=Continue 吞 stderr 合流，成败只看 $LASTEXITCODE），
    # 但脚本经临时文件下发——碎裂根因与修法见 Invoke-GitBashSh 头注
    param([string]$BashExe, [string]$ScriptText)
    $eap = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { Invoke-GitBashSh -BashExe $BashExe -ScriptText $ScriptText 2>&1 | Out-Null } finally { $ErrorActionPreference = $eap }
    return ($LASTEXITCODE -eq 0)
}
function Invoke-WslSh {
    # WSL 壳传输：wsl.exe 对 argv 再重组 + WSL 默认 shell 二次解析，同一碎裂更甚。
    # 脚本串经 stdin 灌入发行版内固定路径 /tmp/ca_bootstrap_run.sh——cat 先整体
    # 耗尽 stdin 再 exec，杜绝 bash -s 增量读被中间命令（apt/git）吃掉的隐患。
    param([string]$Distro, [string]$ScriptText, [switch]$AsRoot)
    $s = ($ScriptText -replace "`r", "") + "`n"
    if ($AsRoot) { $s | wsl -d $Distro -u root -- sh -c 'cat > /tmp/ca_bootstrap_run.sh && exec bash /tmp/ca_bootstrap_run.sh' }
    else { $s | wsl -d $Distro -- sh -c 'cat > /tmp/ca_bootstrap_run.sh && exec bash /tmp/ca_bootstrap_run.sh' }
}
function Probe-WslSh {
    # 同 Probe 的语义，脚本经 stdin 下发——见 Invoke-WslSh 头注
    param([string]$Distro, [string]$ScriptText)
    $eap = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { Invoke-WslSh -Distro $Distro -ScriptText $ScriptText 2>&1 | Out-Null } finally { $ErrorActionPreference = $eap }
    return ($LASTEXITCODE -eq 0)
}

# ---- [1/3] Docker Desktop（Windows 侧，两壳共用同一 daemon）--------------------
Say "== [1/3] Docker Desktop（Windows 侧）=="
if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Say "未检测到 docker CLI —— 经 winget 安装 Docker Desktop..."
    if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
        Die "本机无 winget（App Installer）——请手工安装 Docker Desktop（https://www.docker.com/products/docker-desktop/）并启动后重跑。"
    }
    winget install -e --id Docker.DockerDesktop --accept-source-agreements --accept-package-agreements
    if ($LASTEXITCODE -ne 0) { Die "Docker Desktop 安装失败，请手工安装后重跑。" }
    Die "请启动 Docker Desktop（桌面图标，等右下角鲸鱼图标稳定），然后重跑本脚本。"
}
if (-not (Probe docker version --format '{{.Server.Version}}')) {
    Die "docker daemon 不可达 —— 请启动 Docker Desktop（桌面图标，等鲸鱼图标稳定）后重跑。"
}
Say "docker daemon OK"

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
    # ================= Git Bash 壳（仓库在 NTFS，免管理员[需 Docker 已就绪]）======
    Say "命中 Git Bash：$gitBash"
    if (-not $gitExe) { $gitExe = $gitBash -replace '\\bin\\bash\.exe$', '\cmd\git.exe' }
    if (-not $Dir) { $Dir = Join-Path $env:USERPROFILE "codeaudit-umbrella" }
    $posix = To-PosixPath $Dir
    # 值全部经环境变量下发，bash 侧单引号静态脚本——路径含空格/单引号安全
    $env:CA_REPO_DIR = $posix
    $env:CA_ACTION = $Action

    if (-not (Test-Path (Join-Path $Dir ".git"))) {
        if (-not $RepoUrl) { Die "首次使用请提供 -RepoUrl <伞仓 git 地址>。" }
        Say "克隆（NTFS，core.autocrlf=false 防 CRLF）..."
        & $gitExe clone -c core.autocrlf=false --recurse-submodules $RepoUrl $Dir
        if ($LASTEXITCODE -ne 0) { Die "克隆失败（检查 -RepoUrl 与网络）。" }
    }
    & $gitExe -C $Dir -c core.autocrlf=false submodule update --init --recursive
    if ($LASTEXITCODE -ne 0) { Die "子模块初始化/更新失败（检查网络与子仓可达性）。" }
    # autocrlf=false 无条件落盘伞仓+子仓：克隆/更新时子仓曾被用户全局
    # 配置 smudge，这里之后任何 git 操作不再按全局配置转换
    & $gitExe -C $Dir config core.autocrlf false
    if ($LASTEXITCODE -ne 0) { Die "git config core.autocrlf 失败（仓库状态异常）。" }
    & $gitExe -C $Dir submodule --quiet foreach --recursive 'git config core.autocrlf false' | Out-Null
    if ($LASTEXITCODE -ne 0) { Die "子仓 core.autocrlf 落盘失败（子仓状态异常，检查 git submodule status）。" }

    # CRLF 哨兵：crlf_check.sh 全量扫伞仓+子仓的跟踪 .sh/Dockerfile*
    # 退出码契约：0 干净 / 1 命中 / 2 用法环境错 / 3 真实内容脏拒修；9=cd 失败（本行置位）
    Invoke-GitBashSh -BashExe $gitBash -ScriptText 'cd "$CA_REPO_DIR" || exit 9; sh deploy/windows/crlf_check.sh "$CA_REPO_DIR"'
    if ($LASTEXITCODE -eq 9) { Die "仓库路径不可达（cd 失败）——检查 -Dir 是否正确（当前：$Dir）。" }
    if ($LASTEXITCODE -eq 2) { Die "crlf_check.sh 用法/环境错误（git 缺失或缺参），详见上方输出。" }
    if ($LASTEXITCODE -eq 1) {
        Say "检出含 CRLF —— 归一化为 LF（autocrlf=false + checkout-index 强制重检出）..."
        # 修复守卫内建于 crlf_check.sh --repair：与行尾无关的未提交改动退出 3 拒修
        # （防丢数据），纯行尾脏（re-smudge 形态）自动归一且不丢内容
        Invoke-GitBashSh -BashExe $gitBash -ScriptText 'cd "$CA_REPO_DIR" && sh deploy/windows/crlf_check.sh --repair "$CA_REPO_DIR"'
        if ($LASTEXITCODE -eq 3) { Die "存在与行尾无关的未提交改动，CRLF 修复会丢弃它们——请 stash（含各子仓）后重跑；勿直接 commit（autocrlf=false 下会把 CRLF 写进仓库）。" }
        if ($LASTEXITCODE -ne 0) { Die "CRLF 归一化失败，请手工重克隆（-c core.autocrlf=false）。" }
        Invoke-GitBashSh -BashExe $gitBash -ScriptText 'cd "$CA_REPO_DIR" && sh deploy/windows/crlf_check.sh "$CA_REPO_DIR"'
        if ($LASTEXITCODE -ne 0) { Die "CRLF 归一化后复查仍命中，请手工重克隆（-c core.autocrlf=false）。" }
        Say "CRLF 已归一化"
    }

    # python（商店占位 stub 在 command -v 命中但执行必败——以真实执行为判据）
    $hasPy3 = Probe-GitBashSh -BashExe $gitBash -ScriptText 'command -v python3 >/dev/null 2>&1 && python3 -c "import sys" >/dev/null 2>&1'
    $hasPy  = Probe-GitBashSh -BashExe $gitBash -ScriptText 'command -v python  >/dev/null 2>&1 && python  -c "import sys" >/dev/null 2>&1'
    if (-not $hasPy3 -and -not $hasPy) {
        if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
            Die "缺 Python 且本机无 winget —— 请手工安装 Python 3（python.org，勿用商店占位 stub）后重跑。"
        }
        Say "缺 Python —— winget install Python.Python.3.12（装完请重开终端重跑）..."
        winget install -e --id Python.Python.3.12 --accept-source-agreements --accept-package-agreements
        Die "Python 已安装：请重开 PowerShell/Git Bash 后重跑本脚本。"
    }
    if (-not $hasPy3 -and $hasPy) {
        Invoke-GitBashSh -BashExe $gitBash -ScriptText 'mkdir -p ~/bin && printf "#!/bin/sh\nexec python \"\$@\"\n" > ~/bin/python3 && chmod +x ~/bin/python3'
        if ($LASTEXITCODE -ne 0) { Die "~/bin/python3 垫片创建失败。" }
        Say "已建 ~/bin/python3 垫片(指向 python)"
    }
    if (-not (Probe-GitBashSh -BashExe $gitBash -ScriptText 'command -v unzip >/dev/null 2>&1')) {
        Say "△ Git Bash 缺 unzip：仅影响'沙箱素材缺失需全量拉取'的分支（在位即零下载不受影响）。"
    }

    Say "== [3/3] Git Bash 壳执行（$Action）=="
    Invoke-GitBashSh -BashExe $gitBash -ScriptText 'cd "$CA_REPO_DIR" && exec bash deploy/production-deploy.sh "$CA_ACTION"'
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

    # wsl 能力特征检测：inbox wsl（19041）无 --status/--install
    # --no-distribution 旗标，硬跑会以 Invalid command line option 失败并误导为
    # BIOS 虚拟化问题——按 --help 实际能力分派路径
    $wslHelp = ""
    $eap = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { $wslHelp = (wsl --help 2>&1 | Out-String) } finally { $ErrorActionPreference = $eap }

    Say "启用 WSL2 + 发行版 $Distro..."
    if (-not (Probe wsl --status)) {
        if ($wslHelp -match '--no-distribution') {
            Say "WSL 未安装 —— wsl --install --no-distribution（完成后通常需要重启 Windows，然后重跑本脚本）"
            wsl --install --no-distribution
            if ($LASTEXITCODE -ne 0) { Die "WSL 安装失败（确认 BIOS 虚拟化已开启）。" }
            Die "WSL 功能已启用：请重启 Windows 后重跑本脚本。"
        }
        Die "WSL 不可用且 wsl 版本过旧（无 --no-distribution 旗标）——请从 Microsoft Store 更新'适用于 Linux 的 Windows 子系统'后重跑。"
    }
    $eap = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { $raw = wsl -l -q 2>$null | Out-String } finally { $ErrorActionPreference = $eap }
    $distroList = (($raw -replace "`0","") -split "\r?\n") | ForEach-Object { $_.Trim() } | Where-Object { $_ -ne "" }
    if (-not ($distroList -contains $Distro)) {
        Say "安装发行版 $Distro（首次启动需设置 UNIX 用户名/口令）..."
        wsl --install -d $Distro
        if ($LASTEXITCODE -ne 0) { Die "发行版安装失败。可用 wsl -l -o 查看列表后用 -Distro 指定。" }
    }
    # Docker Desktop WSL 集成：发行版内 docker 必须可用
    if (-not (Probe-WslSh -Distro $Distro -ScriptText 'docker version >/dev/null 2>&1')) {
        Die "WSL 发行版 $Distro 内 docker 不可用 —— 打开 Docker Desktop → Settings → Resources → WSL Integration 勾选 $Distro → Apply & Restart，然后重跑本脚本。"
    }
    if (-not $Dir) { $Dir = "~/codeaudit-umbrella" }
    # WSL 不继承 Windows 环境变量——经 WSLENV 白名单下发；bash 侧一律单引号静态
    # 脚本 + "$VAR" 引用（空格/引号安全，无插值注入面）；~ 前缀在
    # bash 侧显式归一化为 $HOME（单引号内不做 tilde 展开）
    $env:WSLENV = ("$($env:WSLENV):CA_DIR:CA_REPO_URL:CA_ACTION").Trim(':')
    $env:CA_DIR = $Dir
    $env:CA_REPO_URL = $RepoUrl
    $env:CA_ACTION = $Action
    $caPre = 'case "$CA_DIR" in "~"*) CA_DIR="$HOME${CA_DIR#\~}";; esac'
    if ($RepoUrl) {
        Invoke-WslSh -Distro $Distro -AsRoot -ScriptText 'command -v git >/dev/null || { apt-get update && apt-get install -y git; }'
        if ($LASTEXITCODE -ne 0) { Die "WSL 发行版 $Distro 内 git 安装失败（检查网络与 apt 源）。" }
        Invoke-WslSh -Distro $Distro -ScriptText ($caPre + '; test -d "$CA_DIR/.git" || git clone -c core.autocrlf=false --recurse-submodules "$CA_REPO_URL" "$CA_DIR"')
        if ($LASTEXITCODE -ne 0) { Die "仓库克隆失败（检查 -RepoUrl 与网络）。" }
    } else {
        if (-not (Probe-WslSh -Distro $Distro -ScriptText ($caPre + '; test -d "$CA_DIR/.git"'))) {
            Die "WSL 内未发现仓库 $Dir —— 首次使用请提供 -RepoUrl。"
        }
        Say "使用 WSL 内已存在的 $Dir"
    }
    # CRLF 防线（：autocrlf=false 无条件落盘伞仓+子仓
    # 后过哨兵，命中走 repair/recheck；退出码契约同 Git Bash 壳，另 8=配置落盘失败
    Invoke-WslSh -Distro $Distro -ScriptText ($caPre + '; cd "$CA_DIR" || exit 9; git config core.autocrlf false && git submodule --quiet foreach --recursive "git config core.autocrlf false" || exit 8; sh deploy/windows/crlf_check.sh "$CA_DIR"')
    if ($LASTEXITCODE -eq 9) { Die "WSL 内仓库路径不可达（cd 失败）——检查 -Dir 是否正确（当前：$Dir）。" }
    if ($LASTEXITCODE -eq 8) { Die "WSL 内 core.autocrlf 落盘失败（仓库状态异常）。" }
    if ($LASTEXITCODE -eq 2) { Die "crlf_check.sh 用法/环境错误（WSL 内 git 缺失？），详见上方输出。" }
    if ($LASTEXITCODE -eq 1) {
        Say "检出含 CRLF —— 归一化为 LF（autocrlf=false + checkout-index 强制重检出）..."
        Invoke-WslSh -Distro $Distro -ScriptText ($caPre + '; cd "$CA_DIR" && sh deploy/windows/crlf_check.sh --repair "$CA_DIR"')
        if ($LASTEXITCODE -eq 3) { Die "存在与行尾无关的未提交改动，CRLF 修复会丢弃它们——请 stash（含各子仓）后重跑；勿直接 commit。" }
        if ($LASTEXITCODE -ne 0) { Die "CRLF 归一化失败，请手工重克隆（-c core.autocrlf=false）。" }
        Invoke-WslSh -Distro $Distro -ScriptText ($caPre + '; cd "$CA_DIR" && sh deploy/windows/crlf_check.sh "$CA_DIR"')
        if ($LASTEXITCODE -ne 0) { Die "CRLF 归一化后复查仍命中，请手工重克隆（-c core.autocrlf=false）。" }
        Say "CRLF 已归一化"
    }
    Say "== [3/3] WSL 壳执行（$Action）=="
    Invoke-WslSh -Distro $Distro -ScriptText ($caPre + '; cd "$CA_DIR" && exec bash deploy/production-deploy.sh "$CA_ACTION"')
    if ($LASTEXITCODE -ne 0) { Die "部署动作 '$Action' 失败（输出见上）。" }
}

if ($Action -eq "deploy") {
    Say ""
    Say "完成。Windows 本机浏览器访问 http://localhost:<控制台口/网关口>（deploy 完成横幅打印实际端口）。"
    Say "局域网其它设备访问需端口转发：管理员运行 deploy\windows\expose-lan.ps1（-Remove 撤销）。"
} else {
    Say "完成：$Action。"
}
