@echo off
REM Atalho para run.ps1. Exemplos: run.bat   run.bat -Test   run.bat -Bench
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0run.ps1" %*
