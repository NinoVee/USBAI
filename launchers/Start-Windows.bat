@echo off
rem Private AI launcher for Windows. Double-click to start.
title Private AI
cd /d "%~dp0"
set "ARCH=x64"
if /I "%PROCESSOR_ARCHITECTURE%"=="ARM64" set "ARCH=arm64"
if /I "%PROCESSOR_ARCHITEW6432%"=="ARM64" set "ARCH=arm64"
set "EXE=%~dp0bin\windows-%ARCH%\privateai.exe"
if not exist "%EXE%" set "EXE=%~dp0bin\windows-x64\privateai.exe"
if not exist "%EXE%" (
  echo Private AI is not installed for Windows on this drive.
  pause
  exit /b 1
)
"%EXE%" %*
