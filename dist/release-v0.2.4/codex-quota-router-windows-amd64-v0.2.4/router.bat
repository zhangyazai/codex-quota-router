@echo off
setlocal EnableExtensions
chcp 65001 >nul 2>&1

set "TASK_NAME=CodexQuotaRouter"
set "WEB_URL=http://127.0.0.1:4000/"
set "HEALTH_URL=http://127.0.0.1:4000/healthz"
set "SOURCE_BIN=%~dp0codex-quota-router.exe"
set "APP_DIR=%LOCALAPPDATA%\CodexQuotaRouter"
set "INSTALL_DIR=%APP_DIR%\bin"
set "TARGET_BIN=%INSTALL_DIR%\codex-quota-router.exe"
set "CONFIG_DIR=%APPDATA%\codex-quota-router"
set "CQR_TASK_NAME=%TASK_NAME%"
set "CQR_WEB_URL=%WEB_URL%"
set "CQR_HEALTH_URL=%HEALTH_URL%"
set "CQR_INSTALL_DIR=%INSTALL_DIR%"
set "CQR_TARGET_BIN=%TARGET_BIN%"

set "COMMAND=%~1"
if not defined COMMAND set "COMMAND=install"

if /I "%COMMAND%"=="install" goto install
if /I "%COMMAND%"=="start" goto start
if /I "%COMMAND%"=="stop" goto stop
if /I "%COMMAND%"=="status" goto status
if /I "%COMMAND%"=="uninstall" goto uninstall
if /I "%COMMAND%"=="purge" goto purge
goto usage

:install
if not exist "%SOURCE_BIN%" (
  echo 未找到 %SOURCE_BIN%
  echo 请先将 Windows 可执行文件放在 router.bat 同一目录。
  exit /b 1
)

if not exist "%INSTALL_DIR%" mkdir "%INSTALL_DIR%"
if errorlevel 1 exit /b 1

powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "$task=Get-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; if($task){Stop-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; foreach($i in 1..50){$task=Get-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; if(-not $task -or $task.State -notin @('Running','Queued')){break}; Start-Sleep -Milliseconds 100}; if($task -and $task.State -in @('Running','Queued')){throw '旧进程未能停止。'}}"
if errorlevel 1 exit /b 1
copy /Y "%SOURCE_BIN%" "%TARGET_BIN%" >nul
if errorlevel 1 exit /b 1

powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "$ErrorActionPreference='Stop'; $user=[Security.Principal.WindowsIdentity]::GetCurrent().Name; $action=New-ScheduledTaskAction -Execute $env:CQR_TARGET_BIN -WorkingDirectory $env:CQR_INSTALL_DIR; $trigger=New-ScheduledTaskTrigger -AtLogOn -User $user; $principal=New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited; $settings=New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1); Register-ScheduledTask -TaskName $env:CQR_TASK_NAME -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Description 'Codex quota router (current user)' -Force | Out-Null; Start-ScheduledTask -TaskName $env:CQR_TASK_NAME"
if errorlevel 1 (
  powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "$task=Get-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; if($task){Stop-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; Unregister-ScheduledTask -TaskName $env:CQR_TASK_NAME -Confirm:$false}"
  echo 注册当前用户计划任务失败，可能被本机策略禁止。
  exit /b 1
)

powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "foreach($i in 1..50){try{$response=Invoke-WebRequest -UseBasicParsing -Uri $env:CQR_HEALTH_URL -TimeoutSec 1; $health=$response.Content | ConvertFrom-Json; $task=Get-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; if($response.StatusCode -eq 200 -and $health.service -eq 'codex-quota-router' -and $task -and $task.State -eq 'Running'){exit 0}}catch{}; Start-Sleep -Milliseconds 200}; exit 1"
if errorlevel 1 (
  powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "$task=Get-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; if($task){Stop-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; Unregister-ScheduledTask -TaskName $env:CQR_TASK_NAME -Confirm:$false}"
  echo 服务启动后未通过健康检查。
  exit /b 1
)

start "" "%WEB_URL%"
echo 已安装并启动。管理页：%WEB_URL%
exit /b 0

:start
powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "$ErrorActionPreference='Stop'; if(-not (Get-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue)){throw '尚未安装，请先运行 install。'}; Start-ScheduledTask -TaskName $env:CQR_TASK_NAME"
if errorlevel 1 exit /b 1
powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "foreach($i in 1..50){try{$response=Invoke-WebRequest -UseBasicParsing -Uri $env:CQR_HEALTH_URL -TimeoutSec 1; $health=$response.Content | ConvertFrom-Json; $task=Get-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; if($response.StatusCode -eq 200 -and $health.service -eq 'codex-quota-router' -and $task -and $task.State -eq 'Running'){exit 0}}catch{}; Start-Sleep -Milliseconds 200}; exit 1"
if errorlevel 1 (
  powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "$task=Get-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; if($task){Stop-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue}"
  echo 服务启动后未通过健康检查。
  exit /b 1
)
start "" "%WEB_URL%"
echo 已启动。管理页：%WEB_URL%
exit /b 0

:stop
powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "$ErrorActionPreference='Stop'; $task=Get-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; if(-not $task){throw '尚未安装。'}; if($task.State -in @('Running','Queued')){Stop-ScheduledTask -TaskName $env:CQR_TASK_NAME}; foreach($i in 1..50){$task=Get-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; if(-not $task -or $task.State -notin @('Running','Queued')){exit 0}; Start-Sleep -Milliseconds 100}; throw '进程未能停止。'"
if errorlevel 1 exit /b 1
echo 已停止。
exit /b 0

:status
powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "$task=Get-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; if(-not $task){Write-Host '未安装。'; exit 1}; try{$response=Invoke-WebRequest -UseBasicParsing -Uri $env:CQR_HEALTH_URL -TimeoutSec 2; $health=$response.Content | ConvertFrom-Json; if($response.StatusCode -eq 200 -and $health.service -eq 'codex-quota-router' -and $task.State -eq 'Running'){Write-Host ('运行中。管理页：' + $env:CQR_WEB_URL); exit 0}}catch{}; Write-Host ('任务状态：{0}；健康检查失败。' -f $task.State); exit 1"
exit /b %errorlevel%

:purge
set "PURGE_CONFIG=1"
goto uninstall

:uninstall
powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "$task=Get-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; if($task){Stop-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; foreach($i in 1..50){$task=Get-ScheduledTask -TaskName $env:CQR_TASK_NAME -ErrorAction SilentlyContinue; if(-not $task -or $task.State -notin @('Running','Queued')){break}; Start-Sleep -Milliseconds 100}; if($task -and $task.State -in @('Running','Queued')){throw '进程未能停止。'}; Unregister-ScheduledTask -TaskName $env:CQR_TASK_NAME -Confirm:$false}"
if errorlevel 1 exit /b 1

if exist "%TARGET_BIN%" del /F /Q "%TARGET_BIN%"
if exist "%TARGET_BIN%" (
  echo 无法删除正在使用的程序文件，请稍后重试。
  exit /b 1
)
rd "%INSTALL_DIR%" >nul 2>&1
rd "%APP_DIR%" >nul 2>&1

if defined PURGE_CONFIG (
  if exist "%CONFIG_DIR%" rd /S /Q "%CONFIG_DIR%"
  if exist "%CONFIG_DIR%" (
    echo 配置目录删除失败：%CONFIG_DIR%
    exit /b 1
  )
  echo 已卸载并删除默认配置。
) else (
  echo 已卸载；配置保留在 %CONFIG_DIR%
)
exit /b 0

:usage
echo 用法：%~nx0 [install^|start^|stop^|status^|uninstall^|purge]
exit /b 2
