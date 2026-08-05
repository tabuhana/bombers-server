@echo off
rem
rem The bombers launcher for Windows — the sibling of the POSIX `bombers` script,
rem so a fresh clone needs exactly one command in PowerShell or cmd:
rem
rem     .\bombers install
rem
rem Type it WITH the .\ — a bare `bombers` in cmd.exe finds the extensionless
rem POSIX script next door, can't execute it, and stops there rather than trying
rem this file. PowerShell resolves either form to this one.
rem
rem It compiles the server, then runs it with whatever you asked for. You never
rem type `go`. After `install` puts bombers on your PATH, this script is out of
rem the picture: you just type `bombers`.
setlocal

set "here=%~dp0"
set "bin=%here%bin\bombers.exe"

rem A missing Go toolchain is only fatal when there's nothing built yet — if a
rem binary is already sitting there, not having a compiler shouldn't stop you
rem running it.
where go >nul 2>nul
if errorlevel 1 goto :nogo

rem Rebuild every time rather than comparing timestamps the way the POSIX script
rem does: Go's build cache makes an unchanged rebuild a couple of seconds, and a
rem staleness check written in batch is a thing that can be subtly wrong forever.
echo bombers: building...
pushd "%here%"
go build -o "%bin%" ./cmd/bombers
if errorlevel 1 goto :buildfailed
popd
goto :run

:buildfailed
popd
>&2 echo bombers: build failed — nothing was changed.
exit /b 1

:nogo
if not exist "%bin%" goto :nogofatal
goto :run

:nogofatal
>&2 echo bombers: Go isn't installed, so the server can't be built.
>&2 echo          Install Go, then run this again: https://go.dev/dl/
exit /b 1

:run
"%bin%" %*
exit /b %errorlevel%
