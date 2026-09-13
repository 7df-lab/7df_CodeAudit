<#
CodeAudit 局域网暴露（WSL2 部署的可选步骤）
====================================================================
WSL2(NAT 模式)下 Docker Desktop 把容器端口发布到 Windows 的 localhost，
但局域网其它设备访问不到——本脚本用 netsh portproxy 把 Windows 宿主 LAN 口
转发到本机 localhost 并放行防火墙，实现 IP+端口直访（无 DNS 依赖）。

用法（管理员 PowerShell）：
  powershell -ExecutionPolicy Bypass -File deploy\windows\expose-lan.ps1                    # 暴露缺省 8088/8090
  powershell ... -Ports 8088,8090,18800                                                     # 自定义端口
  powershell ... -Remove                                                                    # 撤销转发+防火墙规则

安全口径：防火墙规则仅放行 Private/Domain 配置档——公共
Wi-Fi 等/Public 配置档下不入网暴露；首启账号为 admin/admin，暴露前请先改密。
需要 Public 网段暴露时自行 netsh/New-NetFirewallRule（-Profile Any）并自担风险。

Win11 22H2+ 也可改用 WSL 镜像网络模式（%UserProfile%\.wslconfig 设
networkingMode=mirrored 后 wsl --shutdown）替代本脚本——镜像模式下 localhost 与
LAN IP 等价，无需 portproxy。
#>
#Requires -Version 5.1
param(
    [ValidateRange(1, 65535)]
    [int[]]$Ports = @(8088, 8090),
    [switch]$Remove
)
$ErrorActionPreference = "Stop"
function Say($m){ Write-Host "[expose-lan] $m" }
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host "[expose-lan] ERROR: 请以管理员身份运行。" -ForegroundColor Red; exit 1
}

# 退出码逐口核验（审计 P3：原实现 netsh/防火墙失败也报"已暴露"）
$failed = @()
foreach ($p in $Ports) {
    if ($Remove) {
        cmd /c "netsh interface portproxy delete v4tov4 listenaddress=0.0.0.0 listenport=$p >nul 2>&1"
        if ($LASTEXITCODE -ne 0) { Say "△ $p 的 portproxy 不存在或删除失败（netsh 退出码 $LASTEXITCODE，可忽略未建过的情况）" }
        Get-NetFirewallRule -DisplayName "CodeAudit port $p" -ErrorAction SilentlyContinue | Remove-NetFirewallRule
        Say "已撤销 $p 的转发与防火墙规则"
    } else {
        cmd /c "netsh interface portproxy add v4tov4 listenaddress=0.0.0.0 listenport=$p connectaddress=127.0.0.1 connectport=$p >nul 2>&1"
        if ($LASTEXITCODE -ne 0) {
            Write-Host "[expose-lan] ERROR: $p portproxy 添加失败（netsh 退出码 $LASTEXITCODE）" -ForegroundColor Red
            $failed += $p; continue
        }
        try {
            New-NetFirewallRule -DisplayName "CodeAudit port $p" -Direction Inbound -Protocol TCP -LocalPort $p -Action Allow -Profile Private,Domain | Out-Null
        } catch {
            Write-Host "[expose-lan] ERROR: $p 防火墙规则失败：$($_.Exception.Message)" -ForegroundColor Red
            $failed += $p; continue
        }
        Say "已暴露 $p（0.0.0.0:$p → 127.0.0.1:$p，防火墙限 Private/Domain 放行；撤销用 -Remove）"
    }
}
if ($failed.Count -gt 0) {
    Write-Host "[expose-lan] ERROR: 以下端口暴露失败: $($failed -join ', ')" -ForegroundColor Red
    exit 1
}
if (-not $Remove) {
    Say "局域网设备现在可用 http://<本机IP>:<端口> 访问（仅 Private/Domain 网络配置档生效）。"
}
