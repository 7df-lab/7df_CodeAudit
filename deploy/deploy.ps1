<#
deploy.ps1 — CodeAudit Windows 一键部署（PowerShell 原生，2026-10-09 立项）

产品口径（人类指令 2026-10-09）："109 的构建和部署应该用 powershell 脚本，而不是
通过 git 的 bash 运行 sh 脚本。因为要测试的是在 Windows 上的能力。" —— 本文件即
Windows 面的唯一入口：预检→密钥→镜像源探测(CN-CDN 优先)→五段式部署→验收门，
全程 PowerShell 直接驱动 docker compose / docker build，不执行任何 sh。

与 bash 链（deploy/deploy.sh，Linux/macOS 面）的关系：服务定义（各仓 compose 文件）
与参数面（deploy/internal/production.env 模板+键位）仍是唯一 SSOT；本文件只镜像
编排层。沙箱供给面（agent-tools/pdtools/nuclei-templates/dsh-src）由
dsh-pentest-sse/sandbox-artifacts/supply.ps1 自足保障（2026-10-09 v2：预检自动
增量补齐，取代 v1 的报错指路；数据源与 bash fetch 工具件共用——gh-sources.env/
两 sbom/manifest/补丁，换源升版只改共享文件，PS 面零改动）。
v1 边界（诚实账）：
  - sse 三层构建不做 inputs-digest 快路径（docker 层缓存等价达成幂等加速）；
  - 上游镜像预拉用简单循环（Docker Desktop 的 registry-mirrors/代理承担多源）。

用法：
  powershell -ExecutionPolicy Bypass -File deploy\deploy.ps1                # 一键部署
  powershell -ExecutionPolicy Bypass -File deploy\deploy.ps1 status|stop|down [-v]
环境：Windows 10 1809+ / PowerShell 5.1+；Docker Desktop（未启动时自动拉起等待）。
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)][string]$Action = 'deploy',
    [switch]$V,           # down -v
    [switch]$ConfigureOnly # 只生成/收敛 production.env 后退出（bootstrap configure 动作映射）
)
$ErrorActionPreference = 'Continue'   # 原生命令 stderr 在 EAP=Stop 下会转 NativeCommandError 终止（bootstrap 批②判例）——本脚本全程显式查 $LASTEXITCODE + Die
$Root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location $Root
$EnvFile = 'deploy/internal/production.env'
$Template = 'deploy/internal/production.env.template'

function Say([string]$m) { Write-Host "[ps-deploy] $m" }
function Die([string]$m) { Write-Host "[ps-deploy] ERROR: $m" -ForegroundColor Red; exit 1 }

# ---- 0. Docker 引擎（未启动自动拉起——Docker Desktop 是用户级应用不随服务自启） ----
# PS5.1 陷阱：原生命令 stderr 经 2>$null 仍会以 ErrorRecord 成为管线输出（失配时 -not(...) 恒假，
# 109 实录=supervisor 自举/预拉取/引擎判定三处同型跳过）——docker 命令判定一律 *>$null 吞尽 + $LASTEXITCODE
function Test-Engine { docker version --format '{{.Server.Version}}' *> $null; ($LASTEXITCODE -eq 0) }
if (-not (Test-Engine)) {
    Say 'docker daemon 不可达 —— 尝试自动启动 Docker Desktop（上限 300s）...'
    $dd = "$env:ProgramFiles\Docker\Docker\Docker Desktop.exe"
    if (Test-Path $dd) {
        Start-Process -FilePath $dd | Out-Null
        $deadline = (Get-Date).AddSeconds(300)
        while (((Get-Date) -lt $deadline) -and (-not (Test-Engine))) { Start-Sleep -Seconds 10 }
    }
    if (-not (Test-Engine)) { Die 'docker daemon 仍不可达 —— 请手工启动 Docker Desktop 后重跑' }
}
Say 'docker daemon OK'

# ---- 0b. docker.io 出口自检 + CN registry mirrors 注入（毒 DNS 环境直连 registry-1.docker.io 超时；122 同网先例=mirror 三件套）----
$daemonJson = "$env:USERPROFILE\.docker\daemon.json"
$probeImg = 'alpine:3.19'   # 探针兼预热（后续构建要用）
# 本地缓存优先：镜像在位即跳过一切网络探针——凭据助手受损/离线缓存机（109 实录：
# 镜像经中继全在位, 而 docker pull 必碰残废凭据助手）应能全程离线跑通
docker image inspect $probeImg *> $null
if ($LASTEXITCODE -eq 0) { $script:probeSkip = $true } else { docker pull $probeImg 2>$null | Out-Null }
function Restart-DockerDesktop {
    Say '重启 Docker Desktop（配置面变更需后端重读：credsStore/daemon.json 均为启动期装载）...'
    Get-Process 'Docker Desktop' -ErrorAction SilentlyContinue | Stop-Process -Force
    Start-Sleep -Seconds 5
    Start-Process "$env:ProgramFiles\Docker\Docker\Docker Desktop.exe" | Out-Null
    $ddDeadline = (Get-Date).AddSeconds(300)
    while ((Get-Date) -lt $ddDeadline) { if (Test-Engine) { break }; Start-Sleep -Seconds 10 }
    if (-not (Test-Engine)) { Die 'Docker Desktop 重启失败——请手工检查' }
    Say 'Docker Desktop 已重启'
}
# 凭据助手梗死自愈（无条件+幂等，不随探针跳过——buildkit digest 元数据解析等任何网络面都会撞死助手）：
# 自动登录/服务化会话下 docker-credential-desktop 访问 Credential Manager 报 "A specified logon session does
# not exist"。本链全部公开镜像，匿名拉取即可——剥 config.json 的 credsStore 后【必须重启 DD：后端启动期缓存配置】
$cfg = "$env:USERPROFILE\.docker\config.json"
$stripped = $false
if (Test-Path $cfg) {
    $rawCfg = [IO.File]::ReadAllText($cfg, [Text.Encoding]::UTF8)
    if ($rawCfg -match '"credsStore"') {
        $rawCfg = $rawCfg -replace '"credsStore"\s*:\s*"[^"]*"\s*,?\s*', '' -replace ',(\s*[}\]])', '$1'
        try { $null = $rawCfg | ConvertFrom-Json } catch { $rawCfg = '{ "auths": {} }' }   # 剥残了就落最小档
        [IO.File]::WriteAllText($cfg, $rawCfg, (New-Object System.Text.UTF8Encoding($false)))
        Say '已剥 ~/.docker/config.json 的 credsStore（凭据助手梗死，公开镜像匿名拉取）'
        $stripped = $true
    }
}
if ($stripped) { Restart-DockerDesktop }
if (-not $script:probeSkip) {
if ($LASTEXITCODE -ne 0) {
    docker pull $probeImg 2>$null | Out-Null
}
}   # close if (-not probeSkip)
if ($script:probeSkip) {
    Say "$probeImg 本地在位 —— 跳过 docker.io 拉取探针"
} elseif ($LASTEXITCODE -eq 0) {
    Say 'docker.io 出口 OK'
} else {
    Say '△ docker.io 直连不可达 —— 注入 CN registry mirrors 并重启引擎（一次性）'
    $dj = $null
    if (Test-Path $daemonJson) { try { $dj = Get-Content $daemonJson -Raw | ConvertFrom-Json } catch { $dj = $null } }
    if (-not $dj) { $dj = New-Object PSObject }
    if (-not ($dj.PSObject.Properties['registry-mirrors'] -and @($dj.'registry-mirrors').Count -gt 0)) {
        $obj = @{}
        foreach ($p in $dj.PSObject.Properties) { if ($p.Name -ne 'registry-mirrors') { $obj[$p.Name] = $p.Value } }
        $obj['registry-mirrors'] = @('https://docker.m.daocloud.io/', 'https://dockerproxy.com/', 'https://docker.nju.edu.cn/')
        New-Item -ItemType Directory -Force -Path (Split-Path $daemonJson) | Out-Null
        $obj | ConvertTo-Json -Depth 6 | Set-Content -Path $daemonJson -Encoding ASCII
        Say "已写 $daemonJson（mirror×3，合并保留原有键）"
        Restart-DockerDesktop
    } else { Say 'daemon.json 已含 registry-mirrors（沿用现状不重启）' }
    docker pull $probeImg 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) { Die "mirror 路由下 $probeImg 仍拉取失败——三 CN 源均不可达？网络处置后重跑" }
    Say "$probeImg 经 mirror 拉取 OK"
}

# ---- 公共小件 ----
function Get-RandHex([int]$bytes) {
    $b = New-Object byte[] $bytes
    [System.Security.Cryptography.RNGCryptoServiceProvider]::Create().GetBytes($b)
    ($b | ForEach-Object { $_.ToString('x2') }) -join ''
}
function Test-PortListening([int]$p) {
    [bool](Get-NetTCPConnection -State Listen -LocalPort $p -ErrorAction SilentlyContinue)
}
function Test-PortOurs([int]$p) {
    # docker ps 偶发空输出（daemon 忙/引擎刚起容器未列/compose 拉取中断余波）时重试，
    # 避免把自家容器误判异己——109 实录：空输出窗口 >15s 致 18800 误判 Die + 端口误漂移入 env
    foreach ($try in 1..5) {
        $ours = docker ps --format '{{.Names}} {{.Ports}}' 2>$null
        if ($LASTEXITCODE -eq 0 -and $ours) { break }
        Start-Sleep -Seconds 3
    }
    foreach ($l in @($ours)) {
        if ($l -match '^(codeaudit-|docker-gateway-|openshell-manager|openshell-gateway-)' -and $l -notmatch '^codeaudit-sim-' -and $l -match (':' + $p + '(->|-)')) { return $true }
    }
    return $false
}
function Get-AccessIP {
    # 取向优先级：RFC1918 私网+非 vEthernet（物理 LAN 口）> 私网+vEthernet > 其它非环回。
    # 109 实录先后取到 172.18.224.1(WSL vSwitch)/48.0.0.218(VPN 段)——裸"首个非环回"不可靠
    $cands = Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
        Where-Object { $_.IPAddress -notmatch '^(127\.|169\.254\.)' -and $_.InterfaceAlias -notmatch 'Loopback' }
    $rank = {
        param($a)
        $priv = $a.IPAddress -match '^(10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.)'
        $virt = $a.InterfaceAlias -match 'vEthernet'
        if ($priv -and -not $virt) { 0 } elseif ($priv) { 1 } else { 2 }
    }
    $best = $cands | Sort-Object { & $rank $_ } | Select-Object -First 1
    if ($best) { $best.IPAddress } else { '127.0.0.1' }
}
function Set-EnvKv([string]$k, [string]$v) {
    $lines = @(Get-Content $EnvFile -ErrorAction SilentlyContinue)
    $found = $false
    $out = foreach ($l in $lines) {
        if ($l -match ('^' + [regex]::Escape($k) + '=')) { $found = $true; "$k=$v" } else { $l }
    }
    if (-not $found) { $out = @($out) + @("$k=$v") }
    Set-Content -Path $EnvFile -Value $out -Encoding ASCII
}
function Get-EnvKv([string]$k, [string]$d) {
    $l = (Get-Content $EnvFile -ErrorAction SilentlyContinue | Where-Object { $_ -match ('^' + [regex]::Escape($k) + '=') } | Select-Object -First 1)
    if ($l) {
        $v = $l.Substring($k.Length + 1)
        $v = ($v -replace '[\s#].*$', '').Trim()   # 剥行尾注释/空白（env 文件带 # 注释形态）
        if ($v) { $v } else { $d }
    } else { $d }
}
function Wait-Http([string]$name, [string]$url, [int]$timeoutSec, [string]$want = '') {
    Say ("等待 {0} at {1}（上限 {2}s）..." -f $name, $url, $timeoutSec)
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 5 -ErrorAction Stop
            if ($want -eq '' -or $r.Content -match [regex]::Escape($want)) { Say "$name OK"; return }
        } catch { }
        Start-Sleep -Seconds 3
    }
    Die "$name 未在 ${timeoutSec}s 内就绪（docker ps / docker logs 定位）"
}
function Wait-Tcp([string]$name, [int]$port, [int]$timeoutSec) {
    # TCP 存活（与子仓 gateway_lifecycle tcp_ok 同契约）——网关 health server 绑容器内
    # 127.0.0.1:8081（发布口够不到回环），网关存活判定只认主端口 TCP 可连
    Say ("等待 {0}（TCP 127.0.0.1:{1}，上限 {2}s）..." -f $name, $port, $timeoutSec)
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        $c = New-Object Net.Sockets.TcpClient
        try { $null = $c.ConnectAsync('127.0.0.1', $port).Wait(1500); if ($c.Connected) { Say "$name OK"; return } } catch { } finally { $c.Close() }
        Start-Sleep -Seconds 3
    }
    Die "$name 未在 ${timeoutSec}s 内就绪（docker ps / docker logs 定位）"
}
$envMap = @{}   # production.env 读入（compose --env-file 直读，进程内仅作读取面）
function Import-EnvFile { $script:envMap = @{}; (Get-Content $EnvFile -ErrorAction SilentlyContinue) | ForEach-Object { if ($_ -match '^([A-Z0-9_]+)=(.*)$') { $script:envMap[$matches[1]] = $matches[2] } } }

# ---- status / stop / down（compose 直驱，四项目） ----
function Compose-Engine([string[]]$extraArgs) { & docker compose --env-file $EnvFile -f engine/docker-compose.yml -f engine/deploy/docker-compose.deploy.yml --project-directory engine @extraArgs }
function Compose-Manager([string[]]$extraArgs) { Push-Location manager; try { & docker compose --env-file ("../" + $EnvFile) --project-directory deploy -f deploy/docker-compose.yml @extraArgs } finally { Pop-Location } }
# manager 的 --project-directory 必须钉 compose 文件所在 deploy/（子仓 deploy.sh 同用法）——
# build context:.. 相对项目目录解析，钉 . 会把上下文解析到伞根，Dockerfile 永不可达（109 实录）
function Compose-Web([string[]]$extraArgs) { Push-Location web; try { & docker compose --env-file ("../" + $EnvFile) --project-directory . -f docker-compose.yml @extraArgs } finally { Pop-Location } }
function Compose-Gateway([string[]]$extraArgs) { Push-Location openshell-gateway; try { & docker compose --project-directory . -f docker-compose.yml @extraArgs } finally { Pop-Location } }

if ($Action -in @('status', 'stop', 'down')) {
    if (-not (Test-Path $EnvFile)) { Die "缺 $EnvFile（先跑一次 deploy）" }
    Import-EnvFile
    if ($Action -eq 'status') {
        Say '== gateway =='; Compose-Gateway @('ps')
        Say '== manager =='; Compose-Manager @('ps')
        Say '== engine ==';  Compose-Engine @('ps')
        Say '== web ==';     Compose-Web @('ps')
        try { Invoke-WebRequest -Uri ("http://127.0.0.1:{0}/health" -f (Get-EnvKv 'CODEAUDIT_HOST_GATEWAY' '8090')) -UseBasicParsing -TimeoutSec 4 | Out-Null; Say 'engine gateway: OK' } catch { Say 'engine gateway: DOWN' }
        try { Invoke-WebRequest -Uri 'http://127.0.0.1:18800/healthz' -UseBasicParsing -TimeoutSec 4 | Out-Null; Say 'manager: OK' } catch { Say 'manager: DOWN' }
        try { Invoke-WebRequest -Uri ("http://127.0.0.1:{0}/" -f (Get-EnvKv 'CODEAUDIT_WEB_PORT' '8088')) -UseBasicParsing -TimeoutSec 4 | Out-Null; Say 'web: OK' } catch { Say 'web: DOWN' }
        foreach ($n in @('dsh-pentest-base:latest', 'dsh-runtime-layer:latest', 'dsh-pentest-sse:latest')) {
            docker image inspect $n *> $null
            if ($LASTEXITCODE -eq 0) { Say "sandbox ${n}: OK" } else { Say "sandbox ${n}: MISSING" }
        }
    } elseif ($Action -eq 'stop') {
        Compose-Web @('stop'); Compose-Engine @('stop'); Compose-Manager @('stop'); Compose-Gateway @('stop')
        Say '已停栈（卷与配置保留）'
    } else {
        $dv = @('down'); if ($V) { $dv += '-v' }
        Compose-Web $dv; Compose-Engine $dv; Compose-Manager $dv; Compose-Gateway $dv
        Say ("down {0} 完成" -f $(if ($V) { '-v（连卷）' } else { '（卷保留）' }))
    }
    exit 0
}
if ($Action -ne 'deploy') { Die "未知动作 $Action（deploy|status|stop|down）" }

# ════════════════════════════ deploy ════════════════════════════
Say '全量部署开始（PowerShell 原生链）'

# ---- 1. 预检 ----
foreach ($d in @('engine/docker-compose.yml', 'web/package.json', 'manager/openshell_manager/__init__.py',
                 'openshell-gateway/docker-compose.yml', 'dsh-pentest-sse/Dockerfile.base', 'dsh-pentest-sse/Dockerfile.runtime')) {
    if (-not (Test-Path $d)) { Die "子仓缺失: $d（仓库不完整——重新 clone 或用完整发布包）" }
}
$py = Get-Command python -ErrorAction SilentlyContinue
if ($py) {
    & python deploy/internal/check-yaml-dups.py engine/docker-compose.yml engine/deploy/docker-compose.deploy.yml manager/deploy/docker-compose.yml web/docker-compose.yml openshell-gateway/docker-compose.yml
    if ($LASTEXITCODE -ne 0) { Die 'yaml 查重未过' }
    Say 'yaml 查重 OK'
} else { Say '△ 无 python —— 跳过 yaml 查重（建议安装）' }

# 沙箱供给面自足（supply.ps1：PS 原生增量补齐 agent-tools/pdtools/nuclei-templates/dsh-src，
# 在位即跳过、缺啥补啥 + sha 终门——2026-10-09 v2 取代旧「缺 11 项报错指路」门）
$supply = 'dsh-pentest-sse/sandbox-artifacts/supply.ps1'
if (-not (Test-Path $supply)) { Die "缺 $supply（仓库不完整）" }

# ---- 主仓库素材库落位（assets/sandbox 包优先，在线仅兜底——设计档案 docs/designs/sandbox-materials-umbrella-cache.md）----
# 快路径：supply.ps1 verify 全绿（秒级只读）=零解包零下载；红 = 对在位包覆盖式解包
# （绝不先清空——目标域混树内跟踪件，清空即杀子仓 git dirty；旧供给物残留无害，核验全清单制），
# 解包后仍缺/失配 = 交给下方 supply.ps1 增量补齐链在线兜底。落位永不阻塞部署。
$pkgDir = 'assets/sandbox'
if (Test-Path $pkgDir) {
    # 指纹预检（仅警告）：MANIFEST.md 清单指纹 vs 子仓 HEAD blob sha——rev-parse 取内容哈希，
    # 不受工作树 autocrlf smudge 影响（bash 孪生 supply-cache.sh fingerprint_check 同构）
    $mf = Join-Path $pkgDir 'MANIFEST.md'
    if (Test-Path $mf) {
        $mfLines = [IO.File]::ReadAllLines((Resolve-Path $mf).Path, [Text.Encoding]::UTF8)   # UTF-8 显式读（PS5.1 缺省 GBK 行吞并判例）
        foreach ($fp in @(@{ k = 'agent-tools'; s = 'sandbox-artifacts/agent-tools.sha256' },
                          @{ k = 'dsh-src'; s = 'sandbox-artifacts/dsh-src.sha256' },
                          @{ k = 'pdtools+nuclei'; s = 'sandbox-artifacts/tool-sbom.json' })) {
            $want = ''
            foreach ($l in $mfLines) { if ($l -match ('^- ' + [regex]::Escape($fp.k) + ':\s+([0-9a-f]{40})')) { $want = $matches[1]; break } }
            $have = @(git -C dsh-pentest-sse rev-parse ('HEAD:' + $fp.s) 2>$null) -join ''
            if ($want -and $have -and ($want -ne $have)) { Say ("△ 素材包指纹失配: {0}（滞后当前清单，缺口将在线补——建议 make supply-refresh）" -f $fp.k) }
        }
    }
    & powershell -NoProfile -ExecutionPolicy Bypass -File $supply verify *> $null
    if ($LASTEXITCODE -eq 0) {
        Say '素材面全绿（verify 快路径）——零解包零下载'
    } else {
        # tar 钉 System32 全路径（经验⑤：Git Bash 的 GNU tar 把 C:\ 当 host:path；缺省回退探测同 supply.ps1）
        $tarExe = "$env:SystemRoot\System32\tar.exe"
        if (-not (Test-Path $tarExe)) { $c = Get-Command tar.exe -ErrorAction SilentlyContinue; if ($c) { $tarExe = $c.Source } else { $tarExe = $null } }
        if (-not $tarExe) { Say '△ 无 tar.exe（Windows 10 1809+ 自带）——素材包落位跳过，在线链兜底' }
        else {
            foreach ($pkg in @('agent-tools', 'dsh-src', 'pdtools', 'nuclei-templates')) {
                $tgz = Join-Path $pkgDir ($pkg + '.tar.gz')
                if (-not (Test-Path $tgz)) { continue }
                $dst = 'dsh-pentest-sse/sandbox-artifacts/' + $pkg
                if (-not (Test-Path $dst)) { New-Item -ItemType Directory -Force -Path $dst | Out-Null }
                & $tarExe -xzf (Resolve-Path $tgz).Path -C ((Resolve-Path $dst).Path)
                if ($LASTEXITCODE -eq 0) { Say ("素材包落位: {0}" -f $pkg) } else { Say ("△ {0}.tar.gz 解包失败（包损坏？git 对象受损？）——该域交在线链" -f $pkg) }
            }
        }
    }
} else {
    Say '（无 assets/sandbox 素材库——全在线供给）'
}

& powershell -NoProfile -ExecutionPolicy Bypass -File $supply
if ($LASTEXITCODE -ne 0) { Die "沙箱供给面未就绪——见上（手工重试: powershell -ExecutionPolicy Bypass -File $supply）" }

# opengrep（gitignored 大件：在位核验；缺失经多源拉取+sha 复核）
$ogDir = 'engine/services/sast-adapter-service/tools'
$ogPin = (Get-Content "$ogDir/opengrep.sha256" -ErrorAction SilentlyContinue | Select-Object -First 1) -replace '\s.*$', ''
if (Test-Path "$ogDir/opengrep") {
    $ogGot = (Get-FileHash "$ogDir/opengrep" -Algorithm SHA256).Hash.ToLower()
    if ($ogGot -ne $ogPin) { Die 'opengrep sha 漂移——按 PROVENANCE.md 重取' }
    Say 'opengrep 在位（sha OK）'
} else {
    Say 'opengrep 缺失 —— 多源拉取（v1.29.0 manylinux x86）...'
    $ogUrl = 'https://github.com/opengrep/opengrep/releases/download/v1.29.0/opengrep_manylinux_x86'
    # 2026-10-10 经验（与 production-deploy.sh ensure_opengrep 同批）：官方源黑洞连接可
    # 挂满 TimeoutSec 1800——UPSTREAM_MIRROR 设定时提为第一源（dsh-src 同名约定）；
    # -TimeoutSec 240 限单源总时长（46MB 在可用源上远低于此，劣化源快败换下一家）。
    $ogMirror = if ($env:UPSTREAM_MIRROR) { $env:UPSTREAM_MIRROR.TrimEnd('/') } else { '' }
    $sources = @()
    if ($ogMirror) { $sources += $ogMirror }
    $sources += @('', 'https://ghfast.top/', 'https://gh-proxy.com/')
    $ok = $false
    foreach ($src in $sources) {
        try {
            Invoke-WebRequest -Uri ($src + $ogUrl) -OutFile "$ogDir/opengrep" -UseBasicParsing -TimeoutSec 240 -ErrorAction Stop
            $ok = $true; break
        } catch { Say ("  源失败: " + $(if ($src) { $src } else { 'official' })) }
    }
    if (-not $ok) { Die 'opengrep 下载失败（无 GitHub 出口）——按 PROVENANCE.md 手工 vendor' }
    $ogGot = (Get-FileHash "$ogDir/opengrep" -Algorithm SHA256).Hash.ToLower()
    if ($ogGot -ne $ogPin) { Die 'opengrep sha 不符' }
    Say 'opengrep 已拉取并复核'
}

# ---- 2. production.env 生成/收敛 ----
if (-not (Test-Path $EnvFile)) {
    Say "生成 $EnvFile（密钥随机 + 宿主 IP 探测）"
    $jwt = Get-RandHex 24; $tok = Get-RandHex 24; $ip = Get-AccessIP
    # 模板=UTF-8 无 BOM（PS5.1 Get-Content 按 ANSI/GBK 误读→中文占位符乱码→替换永不命中，
    # 109 实录=token/IP 全未替换+行合并）；.NET 显式 UTF-8 读 + String.Replace 字面替换（免正则/免链式 -replace 歧义）
    $lines = [IO.File]::ReadAllLines((Resolve-Path $Template), [Text.Encoding]::UTF8)
    $out = foreach ($l in $lines) {
        $l = $l.Replace('<auto: openssl rand -hex 24，manager 与 engine 同值由本文件保证>', $tok)
        $l = $l.Replace('<auto: openssl rand -hex 24>', $jwt)
        $l = $l.Replace('<auto: <宿主IP>>', $ip)
        $l
    }
    [IO.File]::WriteAllLines((Join-Path (Get-Location).Path $EnvFile), $out)
    Say "已生成（宿主 IP=$ip）"
}
# 联动键收敛（与 bash 链同口径：manager 是内部面非交互面）
Import-EnvFile
$gwPort = $envMap['OPENSHELL_PORT']; if (-not $gwPort) { $gwPort = '8080' }
$engPort = $envMap['CODEAUDIT_HOST_GATEWAY']; if (-not $engPort) { $engPort = '8090' }
Set-EnvKv 'OPENSHELL_MANAGER_URL' 'http://host.docker.internal:18800'
Set-EnvKv 'OPENSHELL_GATEWAY_ENDPOINT' "host.docker.internal:$gwPort"
Set-EnvKv 'CODEAUDIT_GATEWAY_DIAL_ADDR' "host.docker.internal:$gwPort"
Set-EnvKv 'CODEAUDIT_GATEWAY_UPSTREAM' "host.docker.internal:$engPort"
Import-EnvFile

# 端口冲突自愈（非交互档：异己占用自动改口；18800 固定口被异己占用即失败）
$portKeys = @(
    @{k='OPENSHELL_PORT'; d=8080; n='openshell-gateway gRPC'},
    @{k='OPENSHELL_HEALTH_PORT'; d=8081; n='gateway health'},
    @{k='CODEAUDIT_HOST_GATEWAY'; d=8090; n='引擎网关 API'},
    @{k='CODEAUDIT_WEB_PORT'; d=8088; n='web 控制台'},
    @{k='CODEAUDIT_HOST_PG'; d=5432; n='PostgreSQL'},
    @{k='CODEAUDIT_HOST_REDIS'; d=6379; n='Redis'},
    @{k='CODEAUDIT_HOST_MINIO_API'; d=9000; n='MinIO API'},
    @{k='CODEAUDIT_HOST_MINIO_CONSOLE'; d=9001; n='MinIO 控制台'},
    @{k='CODEAUDIT_HOST_KAFKA'; d=9092; n='Kafka'}
)
$planned = @{}
foreach ($pk in $portKeys) { $planned[$pk.k] = [int](Get-EnvKv $pk.k $pk.d) }
# docker 稳定窗：引擎刚起时 wslrelay 端口代理先于容器列表出现，直接判端口会误伤
# 自家回拉容器——等 docker ps 非空（上限 60s）再做端口判定
Say '端口扫描（docker 稳定窗 ≤60s → 9 口探测 → 键收敛回写）...'
$settleDeadline = (Get-Date).AddSeconds(60)
while ((Get-Date) -lt $settleDeadline) {
    $ps1 = docker ps --format '{{.Names}}' 2>$null
    if ($LASTEXITCODE -eq 0 -and $ps1) { break }
    Start-Sleep -Seconds 5
}
if (Test-PortListening 18800 -and -not (Test-PortOurs 18800)) {
    Start-Sleep -Seconds 20   # 判死前宽限：docker ps 空输出瞬态窗口可 >15s（109 实录），20s 后复核
    if (-not (Test-PortOurs 18800)) { Die 'manager 固定发布 18800 且被其他程序占用——请腾出 18800 后重试' }
}
foreach ($pk in $portKeys) {
    $cur = $planned[$pk.k]
    if ((Test-PortListening $cur) -and -not (Test-PortOurs $cur)) {
        $p = $cur
        do { $p++ } while ((Test-PortListening $p) -or ($planned.ContainsValue($p)) -or ($p -eq 18800))
        Set-EnvKv $pk.k "$p"; $planned[$pk.k] = $p
        Say ("△ 端口 {0}({1}) 被其他程序监听 —— 改用 {2}" -f $cur, $pk.n, $p)
    }
}
foreach ($pk in $portKeys) { $cur = $planned[$pk.k]; $dup = @($planned.Values | Where-Object { $_ -eq $cur }); if ($dup.Count -gt 1) { Die "端口表重复: $cur（键 $($pk.k)）——请检查 $EnvFile" } }
Import-EnvFile
Say ("端口表收敛完成：web={0} 引擎={1} 网关={2}" -f $planned['CODEAUDIT_WEB_PORT'], $planned['CODEAUDIT_HOST_GATEWAY'], $planned['OPENSHELL_PORT'])

if ($ConfigureOnly) {
    Say ("参数已确认并落盘 $EnvFile；执行 deploy.ps1 deploy 开始部署。访问入口(显示): http://{0}:{1}" -f (Get-AccessIP), $planned['CODEAUDIT_WEB_PORT'])
    exit 0
}

# ---- 3. 镜像源探测（CN-CDN 优先，国际源回落——与 bash 链同骨架方向相反的先验） ----
function Test-Url([string]$u) { try { Invoke-WebRequest -Uri $u -UseBasicParsing -TimeoutSec 5 -Method Head -ErrorAction Stop | Out-Null; $true } catch { $false } }
if (-not $env:GOPROXY) {
    if (-not (Test-Url 'https://goproxy.cn/')) { $env:GOPROXY = 'https://proxy.golang.org,direct'; Say '△ goproxy.cn 不可达 —— GOPROXY 回落 proxy.golang.org' }
}
if (-not $env:NPM_REGISTRY) {
    if (Test-Url 'https://registry.npmmirror.com/-/ping') { $env:NPM_REGISTRY = 'https://registry.npmmirror.com' }
    else { $env:NPM_REGISTRY = 'https://registry.npmjs.org'; Say '△ npmmirror 不可达 —— NPM 回落 npmjs' }
}
if (-not $env:PIP_INDEX_URL) {
    if (-not (Test-Url 'https://mirrors.aliyun.com/pypi/simple/')) { $env:PIP_INDEX_URL = 'https://pypi.org/simple'; Say '△ 阿里云 pypi 不可达 —— PIP 回落 pypi.org' }
}
Say ("镜像源: GOPROXY={0} NPM={1} PIP={2}" -f $env:GOPROXY, $env:NPM_REGISTRY, $env:PIP_INDEX_URL)

# ---- 4. 上游镜像预拉（daemon mirrors 路由两试 + 显式镜像域名兜底链+retag） ----
# 109 实录：minio/kafka 经 daemon mirrors 偶发 denied，显式限定镜像域名可过；
# 镜像源连接可无限挂死（无超时）——Pull-Bounded 用后台 Job 包一层硬超时，到点即弃该源
function Pull-Bounded([string]$ref, [int]$timeoutSec = 300) {
    $j = Start-Job -ScriptBlock { param($r) docker pull $r *> $null } -ArgumentList $ref
    Wait-Job $j -Timeout $timeoutSec | Out-Null
    if ($j.State -eq 'Running') { Stop-Job $j -ErrorAction SilentlyContinue; Remove-Job $j -Force; return $false }
    Remove-Job $j -Force
    docker image inspect $ref *> $null
    return ($LASTEXITCODE -eq 0)
}
foreach ($img in @('ghcr.io/nvidia/openshell/supervisor@sha256:c8c42aef16c200063e32cbf72e553e4ead027085427b555efafd95063ecead42', 'ghcr.io/nvidia/openshell-community/sandboxes/base@sha256:aeef1c63f00e2913ea002ccb3aaf925f338b5c5d70e63576f0d95c16a138044e',
                   'python:3.12-slim', 'postgres:16-alpine', 'redis:7-alpine', 'minio/minio:latest',
                   'bitnami/kafka:3.7', 'golang:1.22-alpine', 'alpine:3.19', 'node:20-alpine', 'nginx:1.27-alpine')) {   # 集合=与 bash 链 prepull 清单对齐
    docker image inspect $img *> $null
    if ($LASTEXITCODE -eq 0) { continue }
    $got = $false
    foreach ($m in @('', '', 'docker.m.daocloud.io/', 'dockerproxy.com/', 'docker.nju.edu.cn/')) {   # 空串×2=daemon mirrors 路由重试
        $ref = $m + $img
        Say "pull $ref ..."
        $ok = Pull-Bounded $ref 300
        if ($ok) {
            if ($m) { docker tag $ref $img; Say "  $m → $img retag OK" }
            $got = $true; break
        }
        Say "  源失败/超时（300s），下一源"
        Start-Sleep -Seconds 5
    }
    if (-not $got) { Say "△ pull $img 全源失败（compose up 时再试）" }
}

# ---- [1/5] openshell-gateway ----
Say '== [1/5] openshell-gateway（JWT 自举 + server_sans 钉路由域 + compose up）=='
# server_sans 幂等钉 `*.ROUTING_DOMAIN`（纯字符串路由键，零 DNS）
$RoutingDomain = Get-EnvKv 'ROUTING_DOMAIN' 'sandbox.codeaudit.internal'
$toml = 'openshell-gateway/gateway.toml'
$sansLine = 'server_sans = ["*.' + $RoutingDomain + '"]'
# 同 env 生成坑：toml 含中文注释，PS5.1 Get-Content 按 GBK 误读再 Set-Content -Encoding UTF8 回写=逐轮劣化；
# .NET 显式 UTF-8 无 BOM 读写；行尾沿用文件原生 LF（原 `r`n 混入 LF 文件成杂音）
$tcontent = [IO.File]::ReadAllText((Resolve-Path $toml), [Text.Encoding]::UTF8)
$nl = "`n"
if ($tcontent -match "(?m)^server_sans\s*=.*$") {
    $tcontent = $tcontent -replace "(?m)^server_sans\s*=.*$", $sansLine.Replace('$', '$$')
} elseif ($tcontent -match "(?m)^\[openshell\.gateway\]") {
    $tcontent = $tcontent -replace "(?m)^(\[openshell\.gateway\])", ('$1' + $nl + $sansLine)
} else { $tcontent += ($nl + '[openshell.gateway]' + $nl + $sansLine + $nl) }
[IO.File]::WriteAllText((Resolve-Path $toml), $tcontent, (New-Object System.Text.UTF8Encoding($false)))
# JWT 密钥自举（容器面路径，DD-VM 内持久；幂等存在性检查经 alpine test）
$jwtParent = '/var/lib/openshell/tls'
$hasKey = $false
docker run --rm -v "${jwtParent}:${jwtParent}" alpine:3.19 test -f "$jwtParent/jwt/signing.pem" 2>$null
if ($LASTEXITCODE -eq 0) { $hasKey = $true }
if (-not $hasKey) {
    Say 'JWT signing keys absent —— one-shot generate-certs ...'
    docker run --rm --user 0 -v "${jwtParent}:${jwtParent}" `
        'ghcr.io/nvidia/openshell/gateway@sha256:05cf77bbb022a739aed6f22daa0e7e164415f4ab273b5f84319e46d91eb8f645' `
        generate-certs --output-dir $jwtParent --server-san host.openshell.internal
    # 半套 PKI（122 实录：public.pem 在而 signing.pem 缺）——generate-certs 只会 skip 不补私钥；
    # 整目录轮换（挂 PARENT 移子目录避开挂载根不可移，旧套 .stale 留档）后重生成
    docker run --rm -v "${jwtParent}:${jwtParent}" alpine:3.19 test -f "$jwtParent/jwt/signing.pem" 2>$null
    if ($LASTEXITCODE -ne 0) {
        Say "  半套 PKI——轮换 $jwtParent 后重生成（旧套 .stale 留档）"
        $jwtBase = Split-Path $jwtParent
        $ts = [int][double]::Parse((Get-Date -UFormat %s))
        docker run --rm --user 0 -v "${jwtBase}:${jwtBase}" alpine:3.19 sh -c "mv '$jwtParent' '${jwtParent}.stale-$ts'; mkdir -p '$jwtParent'"
        docker run --rm --user 0 -v "${jwtParent}:${jwtParent}" `
            'ghcr.io/nvidia/openshell/gateway@sha256:05cf77bbb022a739aed6f22daa0e7e164415f4ab273b5f84319e46d91eb8f645' `
            generate-certs --output-dir $jwtParent --server-san host.openshell.internal
        if ($LASTEXITCODE -ne 0) { Die 'PKI 轮换后 generate-certs 仍失败' }
        docker run --rm -v "${jwtParent}:${jwtParent}" alpine:3.19 test -f "$jwtParent/jwt/signing.pem" 2>$null
        if ($LASTEXITCODE -ne 0) { Die 'PKI 轮换后 signing.pem 仍缺失' }
        Say '  PKI 轮换完成'
    }
}
# supervisor 镜像自举——真配对 digest 钉版：
# 钉版网关(09-17) 的真搭档=2026-08-28 构建(:latest 当期, 根提取位原生+老 CLI 契约+携带 sandbox principal
# 出示能力)；上游 :latest(09-28 起)换新契约(要求 --backend-descriptor-file)且旧版无 tag 存档→
# digest 钉 c8c42aef(不可变永可拉)。4 月版本线 0.0.3x 虽老契约但无 principal 出示→log-push 恒 403(122 实测)。
$supRef = [regex]::Match($tcontent, '(?m)^\s*supervisor_image\s*=\s*"([^"]+)"').Groups[1].Value
if (-not $supRef) { Die 'gateway.toml 未解析到 supervisor_image（钉值漂移？）——部署中止' }
$supEraTag = 'ghcr.io/nvidia/openshell/supervisor:era'      # 本地缓存键（中继/装载面用）
$supEraDigest = 'ghcr.io/nvidia/openshell/supervisor@sha256:c8c42aef16c200063e32cbf72e553e4ead027085427b555efafd95063ecead42'
docker image inspect $supRef *> $null
if ($LASTEXITCODE -ne 0) {
    docker image inspect $supEraTag *> $null
    if ($LASTEXITCODE -ne 0) {
        Say "supervisor 自举：pull 真配对 era digest（2026-08-28 构建，与钉版网关同代）..."
        $ok = Pull-Bounded $supEraDigest 600
        if (-not $ok) { Die "supervisor era 拉取失败（$supEraDigest）——ghcr.io 出口问题；网关必 crash loop，部署中止" }
        docker tag $supEraDigest $supEraTag
    }
    docker tag $supEraTag $supRef
    Say "supervisor era 配对 OK → $supRef（digest 钉版，根提取位原生在位）"
    # 网关在启动期提取 supervisor 二进制——era 更换后运行中容器仍用旧缓存，必须 force-recreate
    $gcid = docker ps -q --filter 'name=openshell-gateway-gateway'
    if ($gcid) { docker rm -f $gcid *> $null; Say '  网关容器已重建（拾取 era supervisor）' }
}
# （旧名别名兼容层已退役——era 钉版方案在上一步直接构建根路径兼容镜像，覆盖其全部职能）
$env:OPENSHELL_PORT = $planned['OPENSHELL_PORT'].ToString(); $env:OPENSHELL_HEALTH_PORT = $planned['OPENSHELL_HEALTH_PORT'].ToString()
Compose-Gateway @('up', '-d')
if ($LASTEXITCODE -ne 0) { Die 'gateway compose up 失败' }
Wait-Tcp 'gateway liveness' ([int]$planned['OPENSHELL_PORT']) 120

# ---- [2/5] openshell-manager ----
Say '== [2/5] openshell-manager（config.json 物化 + .env 派生 + compose build/up）=='
if (-not (Test-Path 'manager/config.json')) {
    Copy-Item 'manager/deploy/config.template.json' 'manager/config.json'
    Say 'manager: config.json ← deploy/config.template.json'
}
$tok = Get-EnvKv 'OPENSHELL_MANAGER_TOKEN' ''
@("OPENSHELL_MANAGER_TOKEN=$tok", ("OPENSHELL_GATEWAY_ENDPOINT=host.docker.internal:" + $planned['OPENSHELL_PORT'])) |
    Set-Content -Path 'manager/.env' -Encoding ASCII
Compose-Manager @('up', '-d', '--build')
if ($LASTEXITCODE -ne 0) { Die 'manager compose up 失败' }
Wait-Http 'manager healthz' 'http://127.0.0.1:18800/healthz' 180

# ---- [3/5] engine（7 服务 + 4 中间件） ----
$engTag = ''
try { $engTag = (git -C engine rev-parse --short HEAD 2>$null) } catch { }
if (-not $engTag -and (Test-Path 'deploy/internal/.buildrev')) {
    $engTag = (Get-Content 'deploy/internal/.buildrev' | Where-Object { $_ -match '^BUILDREV_ENGINE=' }) -replace '^BUILDREV_ENGINE=', ''
}
if (-not $engTag) { $engTag = 'latest' }
$env:ENGINE_IMAGE_TAG = $engTag
Say ("== [3/5] engine（tag=$engTag）==")
Compose-Engine @('down', '--remove-orphans') | Out-Null
Compose-Engine @('up', '-d', '--build')
if ($LASTEXITCODE -ne 0) { Die 'engine compose up 失败' }
# pg 就绪 + 空库初始化（服务自带 CREATE TABLE IF NOT EXISTS 自迁移）
$deadline = (Get-Date).AddSeconds(120)
while ((Get-Date) -lt $deadline) {
    $h = docker inspect -f '{{.State.Health.Status}}' codeaudit-postgres 2>$null
    if ($h -match 'healthy') { break }
    Start-Sleep -Seconds 3
}
$initSql = "SELECT 'CREATE DATABASE ' || d || ' OWNER postgres;' FROM (VALUES ('codeaudit_project'),('codeaudit_task'),('codeaudit_result')) AS v(d) WHERE NOT EXISTS (SELECT from pg_database WHERE datname = v.d)"
$initSql | docker exec -i codeaudit-postgres psql -U postgres -tA | Out-Null
$initSql | docker exec -i codeaudit-postgres psql -U postgres | Out-Null
Say 'engine 空库初始化 OK'
Wait-Http 'engine gateway' ("http://127.0.0.1:{0}/health" -f $planned['CODEAUDIT_HOST_GATEWAY']) 240
# 服务面健康门：11 容器 running + 网络端点在位
$deadline = (Get-Date).AddSeconds(180); $bad = ''
while ((Get-Date) -lt $deadline) {
    $bad = ''
    foreach ($c in @('codeaudit-gateway', 'codeaudit-project', 'codeaudit-task', 'codeaudit-storage',
                     'codeaudit-result', 'codeaudit-sast-adapter', 'codeaudit-runtime',
                     'codeaudit-postgres', 'codeaudit-redis', 'codeaudit-kafka', 'codeaudit-minio')) {
        $run = docker inspect -f '{{.State.Running}}' $c 2>$null
        if ($run -ne 'true') { $bad += " $c(not-running)"; continue }
        $ep = docker inspect -f '{{range .NetworkSettings.Networks}}{{.NetworkID}} {{end}}' $c 2>$null
        if (-not $ep -or $ep.Trim() -eq '') { $bad += " $c(no-endpoint)" }
    }
    if ($bad -eq '') { break }
    Start-Sleep -Seconds 5
}
if ($bad -ne '') { Die "服务面健康门未过:$bad" }
Say '✓ 服务面健康门：11 容器 running 且网络端点在位'

# 通知消费链金丝雀（kafka→storage→notification 回环；admin 口令已改则跳过）
$canaryTok = ''
try {
    $clr = Invoke-RestMethod -Uri ("http://127.0.0.1:{0}/v1/auth/login" -f $planned['CODEAUDIT_HOST_GATEWAY']) -Method Post `
        -ContentType 'application/json' -Body '{"username":"admin","password":"admin"}' -TimeoutSec 8
    $canaryTok = $clr.access_token
} catch { }
if (-not $canaryTok) { Say '△ admin 口令已非缺省 —— 跳过通知链金丝雀' }
else {
    $canaryId = 'ps-canary-' + (Get-Date -UFormat %s)
    $canaryBody = ('{{"created_by":"user-001","status":"TASK_STATUS_COMPLETED","task_id":"{0}","project_id":"canary","completed_at":{1}}}' -f $canaryId, [int][double]::Parse((Get-Date -UFormat %s)))
    $canaryBody | docker exec -i codeaudit-kafka /opt/bitnami/kafka/bin/kafka-console-producer.sh --bootstrap-server localhost:9092 --topic task.completed 2>$null
    $canaryHit = $false
    foreach ($i in 1..12) {
        Start-Sleep -Seconds 3
        try {
            $ns = Invoke-RestMethod -Uri ("http://127.0.0.1:{0}/v1/notifications?user_id=user-001" -f $planned['CODEAUDIT_HOST_GATEWAY']) `
                -Headers @{ Authorization = "Bearer $canaryTok" } -TimeoutSec 8
            if (@($ns.notifications | Where-Object { $_.body -match $canaryId }).Count -ge 1) { $canaryHit = $true; break }
        } catch { }
    }
    if ($canaryHit) { Say '✓ 通知消费链 OK（金丝雀回环）' }
    else { Say '△ 通知消费链金丝雀未回环（两轮未达）——docker logs codeaudit-storage 定位' }
}

# ---- [4/5] dsh-pentest-sse 沙箱镜像链（三层；docker build 直驱） ----
Say '== [4/5] dsh-pentest-sse 沙箱镜像链（base→runtime→sse + manager 冒烟）=='
function New-Stage([string]$name) {
    $p = Join-Path $env:TEMP "ca-stage-$name-$(Get-Random)"
    New-Item -ItemType Directory -Path $p | Out-Null
    return $p
}
function Stage-RepoFiles([string]$stage) {
    # 本仓配方（除 sandbox-artifacts）→ stage/dsh-pentest-sse/（git archive 优先：权限位 canonical）
    $dst = Join-Path $stage 'dsh-pentest-sse'
    New-Item -ItemType Directory -Path $dst -Force | Out-Null
    # 2026-10-10 经验（109 发布验证实录）：相对 Push 在调用方已入 dsh-pentest-sse 时双重
    # 解析（...\dsh-pentest-sse\dsh-pentest-sse 不存在即红）——改 $PSScriptRoot 绝对锚定，
    # 与调用方 cwd 解耦
    Push-Location (Join-Path $PSScriptRoot '..\dsh-pentest-sse')
    try {
        if (Test-Path .git) {
            $tar = Join-Path $env:TEMP "ca-repo-$(Get-Random).tar"
            git -c core.autocrlf=false archive --output=$tar HEAD   # 钉 false：Windows 全局 autocrlf=true 会把
            if ($LASTEXITCODE -ne 0) { Die 'git archive 配方失败' }  # 归档染 CRLF，镜像内 grep -qx 整行断言炸（109 判例同 bash 链）
            tar -xf $tar -C $dst
            Remove-Item $tar
        } else {
            robocopy . $dst /E /XD .git node_modules __pycache__ sandbox-artifacts /XF *.log | Out-Null
        }
    } finally { Pop-Location }
}
Push-Location 'dsh-pentest-sse'
try {
    # base 层（context = sandbox-artifacts + 配方）
    $s1 = New-Stage base
    Stage-RepoFiles $s1
    robocopy sandbox-artifacts (Join-Path $s1 'sandbox-artifacts') /E /XD dsh-src | Out-Null
    # 离线底座适配：Dockerfile.base 的 FROM 钉 ghcr digest，buildkit 必做在线元数据解析；镜像若经
    # save/load 中继（RepoDigest 不随 tar 走）则本地解析也 miss。底座镜像已在本地且带 relay tag 时，
    # 改写 staging 副本的 FROM 指本地 tag（staging=特许改动面，同 bash 链；镜像身份=自钉 digest 经中继而来）
    $dockerfileBase = Join-Path $s1 'dsh-pentest-sse/Dockerfile.base'
    docker image inspect sandboxes-base-relay:local *> $null
    if ($LASTEXITCODE -eq 0) {
        $dfb = [IO.File]::ReadAllLines($dockerfileBase, [Text.Encoding]::UTF8)
        $dfb = $dfb | ForEach-Object { $_ -replace '^FROM ghcr\.io/nvidia/openshell-community/sandboxes/base@sha256:[0-9a-f]+', 'FROM sandboxes-base-relay:local' }
        [IO.File]::WriteAllLines($dockerfileBase, $dfb)
        Say '底座 FROM 已改指本地 relay tag（离线中继形态）'
    }
    docker build -t dsh-pentest-base:latest -f $dockerfileBase $s1
    if ($LASTEXITCODE -ne 0) { Die 'dsh-pentest-base 构建失败' }
    Say 'dsh-pentest-base OK'
    # DSH 源版本（git 元数据优先，供给树走内容摘要 12 hex）
    $dshDir = 'sandbox-artifacts/dsh-src'
    $dshSha = ''
    if (Test-Path "$dshDir/.git") { $dshSha = (git -C $dshDir rev-parse HEAD 2>$null) }
    if (-not $dshSha) {
        Say '（DSH 供给树无 git 元数据 —— 用源码内容摘要）'
        $files = Get-ChildItem $dshDir -Recurse -File | Where-Object { $_.FullName -notmatch '\\\.git\\|node_modules|\.toolchain' } | Sort-Object FullName
        $h1 = [System.Security.Cryptography.SHA256]::Create()
        $sb = New-Object System.Text.StringBuilder
        foreach ($f in $files) {
            $fh = [System.BitConverter]::ToString($h1.ComputeHash([System.IO.File]::ReadAllBytes($f.FullName))).Replace('-', '').ToLower()
            [void]$sb.Append($fh).Append(' ').AppendLine($f.FullName.Substring((Resolve-Path $dshDir).Path.Length + 1))
        }
        $dshSha = [System.BitConverter]::ToString($h1.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($sb.ToString()))).Replace('-', '').ToLower().Substring(0, 12)
    }
    # runtime 层（context = dsh-runtime 归档 + 配方）
    $s2 = New-Stage runtime
    robocopy $dshDir (Join-Path $s2 'dsh-runtime') /E /XD .git .toolchain node_modules | Out-Null
    Stage-RepoFiles $s2
    docker build -t dsh-runtime-layer:latest --build-arg "BASE_IMAGE=dsh-pentest-base:latest" `
        --build-arg "DSH_CLIENT_COMMIT_HASH=$dshSha" -f (Join-Path $s2 'dsh-pentest-sse/Dockerfile.runtime') $s2
    if ($LASTEXITCODE -ne 0) { Die 'dsh-runtime-layer 构建失败' }
    docker tag dsh-runtime-layer:latest ("dsh-runtime-layer:dsh-{0}" -f $dshSha.Substring(0, [Math]::Min(12, $dshSha.Length)))
    Say 'dsh-runtime-layer OK'
    # sse 顶层（context = 配方 only）
    $s3 = New-Stage top
    Stage-RepoFiles $s3
    docker build -t dsh-pentest-sse:latest --build-arg "BASE_IMAGE=dsh-pentest-base:latest" `
        --build-arg "RUNTIME_IMAGE=dsh-runtime-layer:latest" -f (Join-Path $s3 'dsh-pentest-sse/Dockerfile') $s3
    if ($LASTEXITCODE -ne 0) { Die 'dsh-pentest-sse 顶层构建失败' }
    Say 'dsh-pentest-sse OK'
    # manager 冒烟（忠实复刻 dsh-pentest-sse/deploy.sh 靶式：/api/v1/sandboxes 前缀+spec.template.image+
    # name 路由 wait-ready/services/delete + sandbox_id 路由 exec + bridge healthz 哨兵；原 PS 版打 /v1/sandboxes
    # 少 /api 段恒 404——109 十五~十八轮实录）
    $mgrBase = 'http://127.0.0.1:18800'
    $hdr = @{ Authorization = "Bearer $tok" }
    $sname = 'ps-smoke-' + (Get-Random)
    $sid = ''
    $recycle = {
        if ($sname) {
            try { Invoke-RestMethod -Uri "$mgrBase/api/v1/sandboxes/$sname`?workspace=default" -Method Delete -Headers $hdr -TimeoutSec 30 | Out-Null } catch { }
        }
    }
    try {
        $r = Invoke-RestMethod -Uri "$mgrBase/api/v1/sandboxes" -Method Post -Headers $hdr -ContentType 'application/json' `
            -Body (('{"workspace":"default","name":"' + $sname + '","spec":{"template":{"image":"dsh-pentest-sse:latest"}}}')) -TimeoutSec 30
        $sid = $r.id
        Say "smoke: 沙箱已创建 $sname"
        $null = Invoke-RestMethod -Uri "$mgrBase/api/v1/sandboxes/$sname/wait-ready" -Method Post -Headers $hdr -ContentType 'application/json' `
            -Body '{"workspace":"default","timeout_seconds":180}' -TimeoutSec 200
        Say 'smoke: wait-ready OK'
        $cmd = 'node --version && nohup node /opt/bridge/bridge.mjs >/tmp/bridge.log 2>&1 & sleep 3; curl -fsS -m 3 http://127.0.0.1:8080/healthz && echo __BRIDGE_HEALTHZ_OK__'
        $eb = (('{"sandbox_id":"' + $sid + '","command":["bash","-c","' + $cmd.Replace('"','\"') + '"],"timeout_seconds":60}'))
        $er = Invoke-RestMethod -Uri "$mgrBase/api/v1/sandboxes/exec" -Method Post -Headers $hdr -ContentType 'application/json' -Body $eb -TimeoutSec 90
        $eo = ($er | ConvertTo-Json -Depth 6 -Compress)
        if ($eo -match '__BRIDGE_HEALTHZ_OK__') { Say 'smoke: exec OK（bridge healthz 哨兵）' }
        else { Say "smoke: exec 输出无哨兵（$($eo.Substring(0, [Math]::Min(120, $eo.Length)))）" }
        $sr = Invoke-RestMethod -Uri "$mgrBase/api/v1/sandboxes/$sname/services" -Method Post -Headers $hdr -ContentType 'application/json' `
            -Body '{"workspace":"default","service":"bridge","target_port":8080}' -TimeoutSec 30
        $so = ($sr | ConvertTo-Json -Depth 6 -Compress)
        $u = ([regex]::Match($so, '"url":"[^"]+"')).Value
        if ($u) { Say "smoke: expose OK $u" }
        else { Say "smoke: ExposeService 返回异常: $so" }
    } catch { Say ("smoke: manager 面不可达/失败（不阻断部署）: " + $_.Exception.Message) }
    finally { & $recycle; Say 'smoke: 沙箱回收兜底已执行' }
} finally { Pop-Location }

# ---- [5/5] web + 五层验收门 ----
Say '== [5/5] web（compose build/up + 五层验收门）=='
$rev = ''
try { $rev = (git -C web rev-parse --short HEAD 2>$null) } catch { }
if (-not $rev -and (Test-Path 'deploy/internal/.buildrev')) {
    $rev = (Get-Content 'deploy/internal/.buildrev' | Where-Object { $_ -match '^BUILDREV_WEB=' }) -replace '^BUILDREV_WEB=', ''
}
if (-not $rev) { $rev = 'unknown' }
$ctag = $rev; if ($ctag -eq 'unknown') { $ctag = 'latest' }
$env:WEB_IMAGE_TAG = $ctag; $env:GIT_REV = $rev
Compose-Web @('up', '-d', '--build')
if ($LASTEXITCODE -ne 0) { Die 'web compose up 失败' }
$webPort = $planned['CODEAUDIT_WEB_PORT']
$base = "http://127.0.0.1:$webPort"
Wait-Http 'web' "$base/" 240
# ① 认证透传（未认证必须 401）
try { Invoke-WebRequest -Uri "$base/v1/projects" -UseBasicParsing -TimeoutSec 5 | Out-Null; Die 'web /v1 反代异常（未认证应 401）' }
catch { if ($_.Exception.Response.StatusCode.value__ -ne 401) { Die "web /v1 反代异常（期望 401 透传）" } }
Say 'web /v1 反代 OK（401 透传）'
# ② SPA 深链回退
foreach ($p in @('/', '/projects', '/tasks', '/definitely-not-a-route')) {
    try { $r = Invoke-WebRequest -Uri ($base + $p) -UseBasicParsing -TimeoutSec 5 -ErrorAction Stop } catch { Die "web SPA 路由回退异常（GET $p）" }
    if ($r.StatusCode -ne 200) { Die "web SPA 路由回退异常（GET $p -> $($r.StatusCode)）" }
}
# ③ 静态产物真实可达
$body = (Invoke-WebRequest -Uri "$base/" -UseBasicParsing -TimeoutSec 8).Content
if ($body -notmatch 'id="root"') { Die 'web 首页非 SPA 挂载点' }
$asset = [regex]::Match($body, '/assets/[^"]+\.js').Value
if (-not $asset) { Die 'web 首页未引用打包产物' }
try { Invoke-WebRequest -Uri ($base + $asset) -UseBasicParsing -TimeoutSec 8 | Out-Null } catch { Die "web 打包产物不可达（$asset）" }
Say 'web SPA 深链回退 + 静态产物 OK'
# ④ nginx WS 升级头（渲染后的配置断言）
docker exec codeaudit-web sh -c "grep -q 'proxy_set_header Upgrade' /etc/nginx/conf.d/default.conf" 2>$null
if ($LASTEXITCODE -ne 0) { Die 'web nginx 缺 WS 升级头' }
Say 'web nginx WS 升级头 OK'
# ⑤ 认证正向链路 + 上传体上限（admin 缺省口令；已改则跳过——幂等重跑口径）
$token = ''
try {
    $lr = Invoke-RestMethod -Uri ("http://127.0.0.1:{0}/v1/auth/login" -f $planned['CODEAUDIT_HOST_GATEWAY']) -Method Post `
        -ContentType 'application/json' -Body '{"username":"admin","password":"admin"}' -TimeoutSec 8
    $token = $lr.access_token
} catch { }
if (-not $token) {
    Say '△ admin 口令已非缺省 —— 跳过登录链/上传上限验证（幂等重跑口径）'
} else {
    try {
        Invoke-WebRequest -Uri "$base/v1/projects" -Headers @{ Authorization = "Bearer $token" } -UseBasicParsing -TimeoutSec 8 | Out-Null
        Say 'web 反代正向链路 OK（经 web 源登录+带认证 200）'
    } catch { Die 'web 反代带认证访问异常' }
    # 上传上限：2MB 放行 / 101MB 反代层 413（curl.exe 二进制体上载，PS 管道不沾二进制）
    $small = Join-Path $env:TEMP 'ca-up-small.bin'; $big = Join-Path $env:TEMP 'ca-up-big.bin'
    $fs = [System.IO.File]::Create($small); $fs.SetLength(2MB); $fs.Close()
    $fb = [System.IO.File]::Create($big); $fb.SetLength(105906177); $fb.Close()   # 101MiB+1：nginx 101m 限值恰为 105906176,
    # 原写 101MB=限值本体→nginx 放行→引擎无 /v1/uploads 路由 501（109 十七轮实录）；与 bash 链同靶式
    $codeS = & curl.exe -s -o NUL -w '%{http_code}' -m 30 -X POST "$base/v1/uploads" -H "Authorization: Bearer $token" -H 'content-type: application/octet-stream' --data-binary "@$small"
    if ("$codeS" -eq '413') { Die 'web 上传体上限回落缺省（2MB 被拒）' }
    $codeB = & curl.exe -s -o NUL -w '%{http_code}' -m 90 -X POST "$base/v1/uploads" -H "Authorization: Bearer $token" -H 'content-type: application/octet-stream' --data-binary "@$big"
    Remove-Item $small, $big -ErrorAction SilentlyContinue
    if ("$codeB" -ne '413') { Die "web 上传体上限异常（101MB -> $codeB，期望 413）" }
    Say 'web 上传体上限 OK（2MB 放行 / 101MB 反代层 413）'
}

Say '== 全量部署完成（PowerShell 原生链）=='
$ip = Get-AccessIP
Write-Host ''
Write-Host '============================================================'
Write-Host ("部署完成。访问入口（IP+端口直访，无需任何 DNS）：")
Write-Host ("  控制台   http://{0}:{1}   （admin / admin，登录后请立即改密）" -f $ip, $webPort)
Write-Host ("  网关 API http://{0}:{1}/v1  （JWT Bearer）" -f $ip, $planned['CODEAUDIT_HOST_GATEWAY'])
Write-Host '运维：powershell -ExecutionPolicy Bypass -File deploy\deploy.ps1 status|stop|down'
Write-Host '说明：AI 全链需在网关注册推理 provider（LLM key，一次性步骤）'
Write-Host '============================================================'
