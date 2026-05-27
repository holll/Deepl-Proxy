@echo off
setlocal enabledelayedexpansion

REM ======================================================
REM              Auto Multi-Platform Build Script
REM     Includes Versioning (Git Tag + Commit + Time)
REM ======================================================

echo ----------------------------------------------------
echo Building multi-platform binaries...
echo ----------------------------------------------------

REM 获取版本号
for /f "delims=" %%a in ('git describe --tags --abbrev^=0 2^>nul') do set VERSION=%%a
if "%VERSION%"=="" set VERSION=0.0.0

REM 获取提交号
for /f "delims=" %%a in ('git rev-parse --short HEAD') do set COMMIT=%%a

REM 构建时间
for /f "delims=" %%a in ('powershell -command "Get-Date -Format yyyy-MM-dd_HH-mm-ss"') do set BUILDTIME=%%a

echo Version: %VERSION%
echo Commit:  %COMMIT%
echo Time:    %BUILDTIME%
echo.

REM 输出目录
set OUTDIR=dist
if not exist %OUTDIR% mkdir %OUTDIR%

REM 写入版本文件
echo Version=%VERSION%> %OUTDIR%\version.txt
echo Commit=%COMMIT%>> %OUTDIR%\version.txt
echo BuildTime=%BUILDTIME%>> %OUTDIR%\version.txt

REM =======================
REM   目标平台
REM =======================
set TARGETS=^
windows/amd64 ^
linux/amd64

REM ==========================
REM   遍历编译
REM ==========================
for %%T in (%TARGETS%) do (

    for /f "tokens=1,2 delims=/" %%a in ("%%T") do (

        set GOOS=%%a
        set GOARCH=%%b
        set GOARM=
        set CGO_ENABLED=0

        REM 处理 ARMv7
        if "!GOARCH!"=="armv7" (
            set GOARCH=arm
            set GOARM=7
        )

        set EXT=
        if "!GOOS!"=="windows" set EXT=.exe

        set OUTFILE=%OUTDIR%\deepl-proxy_!GOOS!_%%b!EXT!

        echo ---------------------------------------------------
        echo Building !OUTFILE!
        echo ---------------------------------------------------

        go build ^
            -o "!OUTFILE!" ^
            -ldflags "-s -w -X main.Version=%VERSION% -X main.Commit=%COMMIT% -X main.BuildTime=%BUILDTIME%" ^
            .

        if errorlevel 1 (
            echo.
            echo Build failed for !GOOS! / !GOARCH!
            pause
            exit /b 1
        )

        REM =========================
        REM         UPX 压缩
        REM =========================
        where upx >nul 2>nul
        if !errorlevel!==0 (
            echo Compressing with UPX...
            upx --best --lzma "!OUTFILE!"
        ) else (
            echo UPX not found, skipping compression
        )

    )
)

echo.
echo ===========================================
echo All builds completed successfully!
echo Binaries in: %OUTDIR%
echo ===========================================
echo.

pause
