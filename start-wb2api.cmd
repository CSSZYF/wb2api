@echo off
chcp 65001 >nul
rem ============================================================================
rem wb2api Windows 服务化启动脚本（start / stop / status 三件套之一）
rem
rem 用法：双击本文件，或在 cmd 中执行 start-wb2api.cmd
rem       （工作目录自动切到脚本所在目录，与在哪里调用无关）
rem
rem 设计要点：
rem   1) PID 落盘 wb2api.pid（与本脚本同目录），stop / status 脚本据此定位进程；
rem   2) 启动前用 PowerShell 校验「PID 对应进程的可执行文件路径 == 本目录 wb2api.exe」，
rem      陈旧 PID 只清文件、不杀进程，绝不误杀系统上恰好复用了同一 PID 的其它程序；
rem   3) 后台启动用 Start-Process -WindowStyle Hidden，stdout / stderr 重定向到 data\ 下，
rem      启动窗口关掉不影响服务；
rem   4) 配置路径可用环境变量 WB2API_CONFIG 覆盖（默认当前目录 config.json），
rem      多实例 / 多配置场景无需改脚本；
rem   5) 编码：本文件为 UTF-8（无 BOM），第 2 行 chcp 65001 必须在任何非 ASCII 字节之前
rem      执行，中文提示才不乱码；行尾恒为 CRLF（.gitattributes 声明 *.cmd text eol=crlf，
rem      cmd.exe 对 LF-only 批处理的 if 块 / goto 有边缘解析风险）；
rem   6) 正文提示一律用全角括号（），半角 ) 只在 cmd 语法位置出现，避免 if 块提前闭合；
rem      PowerShell 单行命令全部放在 if (...) 块之外，命令内成对括号不参与块解析。
rem ============================================================================
setlocal EnableDelayedExpansion
cd /d "%~dp0"

rem 配置路径：默认 config.json，可用 WB2API_CONFIG 指定（与 stop / status 同一约定）
if not defined WB2API_CONFIG set "WB2API_CONFIG=config.json"

set "WB2API_ROOT=%CD%"
set "WB2API_EXE=%CD%\wb2api.exe"
set "WB2API_PID_FILE=%CD%\wb2api.pid"

if not exist "%WB2API_EXE%" (
  echo [错误] 未找到 wb2api.exe：%WB2API_EXE%
  echo        请先构建：go build -trimpath -ldflags="-s -w" -o wb2api.exe ./cmd/server
  exit /b 1
)

rem 配置缺失不拦截：服务首次启动会自动生成 config.json（含随机 api_key，日志打印一次）
if not exist "%WB2API_CONFIG%" echo [提示] 未找到 %WB2API_CONFIG%，首次启动将自动生成，含随机 api_key，见日志。

rem 已在运行判定：先假定「陈旧」，PowerShell 校验通过才翻成「已在运行」。
rem WB2API_ALIVE 必须在 powershell 之前初始化：set 会把 errorlevel 清零。
set "WB2API_ALIVE=0"
if exist "%WB2API_PID_FILE%" set /p WB2API_PID=<"%WB2API_PID_FILE%"
if exist "%WB2API_PID_FILE%" powershell -NoProfile -Command "try { $p=Get-Process -Id ([int]$env:WB2API_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -eq [IO.Path]::GetFullPath($env:WB2API_EXE)) { exit 0 } } catch {}; exit 1"
if exist "%WB2API_PID_FILE%" if not errorlevel 1 set "WB2API_ALIVE=1"
if "!WB2API_ALIVE!"=="1" echo [提示] wb2api 已在运行，PID=!WB2API_PID!，未重复启动。
if "!WB2API_ALIVE!"=="1" exit /b 0
rem 陈旧 PID：进程已退出，或该 PID 已被系统复用给别的程序 —— 只清文件，不杀进程
if exist "%WB2API_PID_FILE%" del /q "%WB2API_PID_FILE%" >nul 2>&1

rem data\ 是日志重定向目标，必须先存在（服务自身的 state.json / usage.json 也在该目录）
if not exist "data" mkdir "data"

rem 后台启动：-PassThru 取进程对象，PID 写进 PID 文件（供 stop / status 复用）。
rem 配置路径两侧补引号（[char]34 即半角双引号，此处不写字面量以免打断 cmd 的引号配对），
rem 路径含空格时 Go 侧 flag 仍能收到单个参数。
powershell -NoProfile -Command "$q=[string][char]34; $p=Start-Process -FilePath $env:WB2API_EXE -ArgumentList '-config',($q + $env:WB2API_CONFIG + $q) -WorkingDirectory $env:WB2API_ROOT -WindowStyle Hidden -RedirectStandardOutput (Join-Path $env:WB2API_ROOT 'data\server.out.log') -RedirectStandardError (Join-Path $env:WB2API_ROOT 'data\server.err.log') -PassThru; [IO.File]::WriteAllText($env:WB2API_PID_FILE, [string]$p.Id)"

rem 用「PID 文件是否生成」判定启动是否成功：exe 起来了但 PID 写不进去时也要如实报告
if not exist "%WB2API_PID_FILE%" (
  echo [错误] 启动失败：未生成 PID 文件 %WB2API_PID_FILE%
  echo        脚本目录不可写？可先用下面这条只读命令确认是否已有进程跑起来：
  tasklist /FI "IMAGENAME eq wb2api.exe" /NH
  exit /b 1
)

set /p WB2API_PID=<"%WB2API_PID_FILE%"
echo [完成] wb2api 已启动，PID=!WB2API_PID!
echo        日志：data\server.err.log 为启动与运行日志，data\server.out.log 为对话表格日志
echo        面板地址与探活结果：运行 status-wb2api.cmd 查看，按 config.json 的 listen 推导

rem 存活自检：2 秒内就退出说明启动失败（端口被占 / 配置非法等），把日志指给用户
powershell -NoProfile -Command "Start-Sleep -Seconds 2; try { $p=Get-Process -Id ([int]$env:WB2API_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -eq [IO.Path]::GetFullPath($env:WB2API_EXE)) { exit 0 } } catch {}; exit 1"
if not errorlevel 1 echo [完成] 已确认进程存活。
if errorlevel 1 (
  echo [警告] 进程已退出，PID=!WB2API_PID!，启动失败。请查看 data\server.err.log：
  echo        常见原因：端口已被占用、config.json 内容非法、auths 目录不可写。
  del /q "%WB2API_PID_FILE%" >nul 2>&1
  exit /b 1
)
endlocal
