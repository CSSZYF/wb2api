@echo off
chcp 65001 >nul
rem ============================================================================
rem wb2api Windows 服务化停止脚本（与 start-wb2api.cmd / status-wb2api.cmd 配套）
rem
rem 用法：双击本文件，或在 cmd 中执行 stop-wb2api.cmd
rem
rem 设计要点：
rem   1) 防误杀：只有「PID 文件里的 PID 对应进程的可执行文件路径 == 本目录 wb2api.exe」
rem      才动手终止；陈旧 PID（进程已退出，或 PID 被系统复用给别的程序）只清理 PID
rem      文件，绝不 taskkill；
rem   2) 先试优雅、再强制：不带 /F 的 taskkill 只发 WM_CLOSE 关闭请求；控制台程序多数会
rem      当场回绝（此时立即转强制终止，不空等）。若被接受，服务侧 signal.NotifyContext
rem      会收到 CTRL_CLOSE_EVENT（Go 运行时映射为 SIGTERM），先落盘 state.json 再退出。
rem      保留这一步是为了给进程一个自行收尾的机会，不支持时零代价。
rem   3) 强制终止后确认进程确实退出，未退出则报错并保留 PID 文件，便于人工排查；
rem   4) 优雅等待秒数可用 WB2API_STOP_TIMEOUT 覆盖（默认 10 秒，0 = 直接强制终止）；
rem   5) 无 PID 文件时不做「按进程名猜杀」：手工启动的实例请在任务管理器结束。
rem   6) 正文提示一律用全角括号（），半角 ) 只在 cmd 语法位置出现，避免 if 块提前闭合；
rem      PowerShell 单行命令全部放在 if (...) 块之外，命令内成对括号不参与块解析。
rem ============================================================================
setlocal EnableDelayedExpansion
cd /d "%~dp0"

set "WB2API_EXE=%CD%\wb2api.exe"
set "WB2API_PID_FILE=%CD%\wb2api.pid"
if not defined WB2API_STOP_TIMEOUT set "WB2API_STOP_TIMEOUT=10"

if not exist "%WB2API_PID_FILE%" (
  echo [提示] wb2api 未在运行：没有 PID 文件 %WB2API_PID_FILE%
  echo        若实例是手工启动的，本脚本不按进程名猜杀，请在任务管理器结束。
  echo        以下仅为只读查询，本脚本不启动也不终止任何进程：
  tasklist /FI "IMAGENAME eq wb2api.exe" /NH
  exit /b 0
)

rem WB2API_ALIVE 必须在 powershell 之前初始化：set 会把 errorlevel 清零
set "WB2API_ALIVE=0"
set /p WB2API_PID=<"%WB2API_PID_FILE%"
powershell -NoProfile -Command "try { $p=Get-Process -Id ([int]$env:WB2API_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -eq [IO.Path]::GetFullPath($env:WB2API_EXE)) { exit 0 } } catch {}; exit 1"
if not errorlevel 1 set "WB2API_ALIVE=1"
if "!WB2API_ALIVE!"=="0" (
  echo [提示] PID 文件陈旧：PID=!WB2API_PID! 不存在，或对应进程不是本目录的 wb2api.exe
  echo        已清理该文件，未终止任何进程。
  del /q "%WB2API_PID_FILE%" >nul 2>&1
  exit /b 0
)

echo [执行] 正在停止 wb2api，PID=!WB2API_PID! ...
set "WB2API_FORCE=1"
set "WB2API_EXITED=0"
taskkill /PID !WB2API_PID! /T >nul 2>&1
if not errorlevel 1 set "WB2API_FORCE=0"

rem 优雅等待：仅当关闭请求被系统接受才等待。PowerShell 放在普通行，不进 if (...) 块。
if "!WB2API_FORCE!"=="0" powershell -NoProfile -Command "$sec=[int]$env:WB2API_STOP_TIMEOUT; $end=(Get-Date).AddSeconds($sec); while ((Get-Date) -lt $end) { try { $p=Get-Process -Id ([int]$env:WB2API_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -ne [IO.Path]::GetFullPath($env:WB2API_EXE)) { exit 0 } } catch { exit 0 }; Start-Sleep -Milliseconds 300 }; try { $p=Get-Process -Id ([int]$env:WB2API_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -ne [IO.Path]::GetFullPath($env:WB2API_EXE)) { exit 0 } } catch { exit 0 }; exit 1"
if "!WB2API_FORCE!"=="0" if not errorlevel 1 set "WB2API_EXITED=1"
if "!WB2API_EXITED!"=="1" echo [完成] wb2api 已停止，PID=!WB2API_PID!，关闭请求被接受，未强制终止。
if "!WB2API_EXITED!"=="1" echo        说明：该路径下服务收到 CTRL_CLOSE_EVENT，已先落盘 state.json 再退出。
if "!WB2API_EXITED!"=="1" del /q "%WB2API_PID_FILE%" >nul 2>&1
if "!WB2API_EXITED!"=="1" exit /b 0

echo [提示] 转为强制终止 ...
taskkill /PID !WB2API_PID! /T /F >nul 2>&1
powershell -NoProfile -Command "try { $p=Get-Process -Id ([int]$env:WB2API_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -eq [IO.Path]::GetFullPath($env:WB2API_EXE)) { exit 0 } } catch {}; exit 1"
if not errorlevel 1 (
  echo [错误] 无法终止 wb2api，PID=!WB2API_PID!，进程仍在运行，PID 文件保留。
  echo        请用任务管理器确认该进程，或检查是否受权限限制。
  exit /b 1
)
del /q "%WB2API_PID_FILE%" >nul 2>&1
echo [完成] wb2api 已强制停止，PID=!WB2API_PID!。
echo        说明：强制终止不走优雅停机，state.json 最多可能丢失最近 5 秒的状态变动，
echo        即后台落盘间隔；auths 目录下的账号凭证不受影响。
endlocal
