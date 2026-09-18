@echo off
chcp 65001 >nul
rem ============================================================================
rem wb2api Windows 服务化状态脚本（与 start-wb2api.cmd / stop-wb2api.cmd 配套）
rem
rem 用法：双击本文件，或在 cmd 中执行 status-wb2api.cmd
rem
rem 输出：进程状态（PID / exe 路径 / 启动时间 / 运行时长 / 内存）+ /healthz 探活结果
rem 退出码：0 = 在跑且 /healthz 返回 2xx；1 = 未运行或 PID 陈旧；2 = 无法从 config.json
rem         解析出探活地址；7 = 连接失败（进程在跑但端口连不上）；22 = HTTP 503
rem         （进程在跑但当前无可用账号）；其它非 0 = 其它错误
rem
rem 设计要点：
rem   1) 只读：不写、不删 PID 文件，不启动也不终止任何进程；
rem   2) 进程判定与 start / stop 用同一条防误杀校验（PID 对应进程 exe == 本目录 wb2api.exe）；
rem   3) 探活地址不写死端口：优先环境变量 WB2API_HEALTH_URL，否则从 config.json 的 listen
rem      推导（:7863 / 0.0.0.0:7863 / 127.0.0.1:7863 / [::]:7863 均可；监听地址为空或
rem      0.0.0.0 / :: 时回落 127.0.0.1），端口改了也不用手改脚本；
rem   4) 探活用 PowerShell 内置 Invoke-WebRequest，不依赖 curl.exe（Win10 1803 之前不自带）；
rem   5) 正文提示一律用全角括号（），半角 ) 只在 cmd 语法位置出现，避免 if 块提前闭合；
rem      PowerShell 单行命令全部放在 if (...) 块之外，命令内成对括号不参与块解析。
rem ============================================================================
setlocal EnableDelayedExpansion
cd /d "%~dp0"

rem 配置路径：默认 config.json，可用 WB2API_CONFIG 指定（与 start 脚本同一约定）
if not defined WB2API_CONFIG set "WB2API_CONFIG=config.json"
set "WB2API_EXE=%CD%\wb2api.exe"
set "WB2API_PID_FILE=%CD%\wb2api.pid"

if not exist "%WB2API_PID_FILE%" (
  echo [状态] 未运行：没有 PID 文件 %WB2API_PID_FILE%
  exit /b 1
)

rem WB2API_ALIVE 必须在 powershell 之前初始化：set 会把 errorlevel 清零
set "WB2API_ALIVE=0"
set /p WB2API_PID=<"%WB2API_PID_FILE%"
powershell -NoProfile -Command "try { $p=Get-Process -Id ([int]$env:WB2API_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -eq [IO.Path]::GetFullPath($env:WB2API_EXE)) { exit 0 } } catch {}; exit 1"
if not errorlevel 1 set "WB2API_ALIVE=1"
if "!WB2API_ALIVE!"=="0" (
  echo [状态] 未运行：PID 文件陈旧，PID=!WB2API_PID! 不存在或对应进程不是本目录的 wb2api.exe
  echo        运行 stop-wb2api.cmd 可清理该文件，不会终止任何进程。
  exit /b 1
)

echo [状态] 运行中：PID=!WB2API_PID!
rem 进程明细：PowerShell 侧只输出 ASCII 标签，中文由 cmd 侧输出，避免编码不一致时乱码
powershell -NoProfile -Command "$p=Get-Process -Id ([int]$env:WB2API_PID) -ErrorAction Stop; Write-Host ('  path   : ' + $p.Path); Write-Host ('  start  : ' + $p.StartTime.ToString('yyyy-MM-dd HH:mm:ss')); Write-Host ('  uptime : ' + [int]((Get-Date) - $p.StartTime).TotalMinutes + ' min'); Write-Host ('  memory : ' + [math]::Round($p.WorkingSet64/1MB,1) + ' MB')"

rem 探活地址推导：从 config.json 的 listen 取端口与主机；host 为空 / 0.0.0.0 / :: 时回落 127.0.0.1。
rem /healthz 恒无鉴权：2xx = 有可服务账号；503 = 进程在跑但当前无可用账号。
powershell -NoProfile -Command "$l=''; $url=$env:WB2API_HEALTH_URL; $base=''; try { $l=[string](ConvertFrom-Json -InputObject (Get-Content -Raw -Encoding UTF8 -LiteralPath $env:WB2API_CONFIG)).listen } catch {}; if ($l -match ':(\d+)\s*$') { $port=$matches[1]; $h=$l.Substring(0,$l.Length-$matches[0].Length).Trim(); $h=$h.Trim('[',']').Trim(); if ($h -eq '' -or $h -eq '0.0.0.0' -or $h -eq '::') { $h='127.0.0.1' } elseif ($h -match ':') { $h='[' + $h + ']' }; $base='http://' + $h + ':' + $port }; if (-not $url -and $base) { $url=$base + '/healthz' }; if (-not $url) { Write-Host '  health : cannot derive health url from config.json listen; set WB2API_HEALTH_URL to override'; exit 2 }; if ($base) { Write-Host ('  panel  : ' + $base + '/panel/') }; Write-Host ('  health : ' + $url); try { $r=Invoke-WebRequest -Uri $url -TimeoutSec 5 -UseBasicParsing; Write-Host ('  http   : ' + [int]$r.StatusCode); Write-Host ('  body   : ' + $r.Content); exit 0 } catch { $resp=$_.Exception.Response; if ($resp) { Write-Host ('  http   : ' + [int]$resp.StatusCode); exit 22 } else { Write-Host ('  error  : ' + $_.Exception.Message); exit 7 } }"
set "WB2API_RC=%ERRORLEVEL%"
if "%WB2API_RC%"=="22" echo        HTTP 503：进程在跑，但当前无可用账号，登录或等签到后即可服务。
if "%WB2API_RC%"=="7" echo        连接失败：进程在跑但端口连不上，请确认 config.json 的 listen 未被占用或改过。
if "%WB2API_RC%"=="2" echo        无法推导探活地址：请设置 WB2API_HEALTH_URL 环境变量指定完整 URL。
exit /b %WB2API_RC%
