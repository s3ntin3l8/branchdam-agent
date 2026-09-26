; branchDAM Agent NSIS Installer
; Minimal installer: extract files, create shortcuts, write starter config.
; Requires NSIS 3.x (https://nsis.sourceforge.io)

!include "MUI2.nsh"
!include "FileFunc.nsh"
!include "WinMessages.nsh"

; --- Version (populated by CI) ---
; VERSION is the human-readable release version with any leading "v"
; stripped (e.g. "1.6.0", or "ci-check"/"manual-test" for a non-release
; build) -- used for DisplayVersion and the version-resource
; VIAddVersionKey fields, where a non-numeric string is harmless.
; PRODUCT_VERSION_QUAD is the strict X.X.X.X numeric form VIProductVersion
; itself requires (it rejects anything else outright) -- CI computes it via
; `go run ./tools/winversion "$RELEASE_VERSION"`, which reuses
; internal/appbundle.BundleVersion's own tag-normalization (the same logic
; the macOS Info.plist build already relies on) rather than re-deriving
; version parsing a third time in NSIS's own preprocessor language. Both
; default to "0.0.0"/"0.0.0.0" when not supplied (e.g. a local `makensis`
; run with no -D flags) -- the version resource still exists in that case,
; just zeroed, which keeps a bare local compile working rather than a hard
; compile error.
!ifndef VERSION
    !define VERSION "0.0.0"
!endif
!ifndef PRODUCT_VERSION_QUAD
    !define PRODUCT_VERSION_QUAD "0.0.0.0"
!endif
!define PRODUCT_NAME "branchDAM Agent"
!define PRODUCT_PUBLISHER "branchDAM"
!define PRODUCT_WEB_SITE "https://github.com/s3ntin3l8/branchdam-agent"
!define INSTALL_DIR "$LOCALAPPDATA\Programs\branchDAM"

Name "${PRODUCT_NAME}"
OutFile "branchdam-agent-setup.exe"
InstallDir "${INSTALL_DIR}"
InstallDirRegKey HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "InstallLocation"
RequestExecutionLevel user
Unicode True

; --- Version resource (Explorer Properties > Details, Add/Remove Programs) ---
VIProductVersion "${PRODUCT_VERSION_QUAD}"
VIFileVersion "${PRODUCT_VERSION_QUAD}"
VIAddVersionKey "ProductName" "${PRODUCT_NAME}"
VIAddVersionKey "CompanyName" "${PRODUCT_PUBLISHER}"
VIAddVersionKey "LegalCopyright" "${PRODUCT_PUBLISHER}"
VIAddVersionKey "FileDescription" "${PRODUCT_NAME} Installer"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "ProductVersion" "${VERSION}"

; --- Interface ---
!define MUI_ABORTWARNING
; MUI_ICON/MUI_UNICON point at dist/icon.ico -- generated at build time by
; `go run ./tools/mkicon` (internal/appicon, issue #201), the same
; generate-don't-commit pattern the macOS .icns build already uses. Both
; the installer and uninstaller windows/taskbar entries use it; the path
; is relative to this .nsi file (installer/windows/), two levels up to the
; repo root's dist/. MUI_HEADERIMAGE_BITMAP/MUI_WELCOMEFINISHPAGE_BITMAP
; (the wizard's header/sidebar banner images) remain MUI2's bundled
; defaults -- out of scope for #201, which is icon-only.
!define MUI_ICON "..\..\dist\icon.ico"
!define MUI_UNICON "..\..\dist\icon.ico"
!define MUI_FINISHPAGE_RUN "$INSTDIR\branchdam-agent-tray.exe"
!define MUI_FINISHPAGE_RUN_PARAMETERS "tray"
!define MUI_FINISHPAGE_RUN_TEXT "Launch branchDAM Agent"

; --- Pages ---
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_LICENSE "..\..\LICENSE"
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

; AGENT_ID holds the default agent ID (computer name, or the fallback
; below) computed once in .onInit. A named Var, rather than one of the
; numbered registers, so it survives untouched through the MUI2 welcome/
; license/directory pages and FileOpen's $0 reuse in Section "Install" --
; unlike $0/$1, no MUI macro or plugin call touches a named Var.
Var AGENT_ID

; --- Installer Init ---
Function .onInit
    ; Per-user shell folders ($SMPROGRAMS, $APPDATA, etc.) -- explicit
    ; rather than relying on RequestExecutionLevel user's implicit default,
    ; since this installer's whole no-elevation design (see INSTALL_DIR
    ; above) depends on every shell-folder reference resolving to the
    ; current user's own folders, not the all-users ones. SetShellVarContext
    ; only works inside a Function/Section, hence here rather than at
    ; top level.
    SetShellVarContext current

    ; Generate default agent ID from computer name
    System::Call 'kernel32::GetComputerName(t .r0, *i 256) i .r1'
    ${If} $1 == 0
        StrCpy $0 "workstation-01"
    ${EndIf}
    StrCpy $AGENT_ID $0
FunctionEnd

; The uninstaller is a separate compiled context from the installer (see
; the CheckExeNotRunning macro below for the same un.-prefix pattern) --
; SetShellVarContext must be set again here, or Delete/RMDir below would
; resolve $SMPROGRAMS/$APPDATA against the all-users folders instead of
; the per-user ones the installer actually wrote to.
Function un.onInit
    SetShellVarContext current
FunctionEnd

; Aborts (with a Retry/Cancel prompt, not a silent failure) if the named
; exe under $INSTDIR is currently running. Overwriting a running .exe via
; a plain File instruction otherwise fails with a Windows sharing
; violation that NSIS surfaces as a generic "can't write" error -- this
; gives the operator an actionable message and a chance to quit the tray
; and retry instead. No plugin dependency: probes via a plain
; kernel32::CreateFile call requesting exclusive write access, which
; Windows only refuses with ERROR_SHARING_VIOLATION (32) when the image is
; currently mapped for execution. Defined once via !macro/un. so both the
; installer and the uninstaller (separate compiled contexts in NSIS, each
; needing its own Function) share one implementation -- see the analogous
; un.onInit split above. Takes the exe's basename via the stack (NSIS's
; standard idiom for a "function with a parameter": Push before Call, the
; function consumes it via Exch/Pop).
!macro CheckExeNotRunning un
Function ${un}CheckExeNotRunning
    Exch $0 ; $0 = exe basename, e.g. "branchdam-agent-tray.exe"
    Push $1
    Push $2
    retry_open:
    IfFileExists "$INSTDIR\$0" 0 cleanup ; nothing installed yet -- nothing to check
    System::Call 'kernel32::CreateFile(t "$INSTDIR\$0", i 0x40000000, i 0, i 0, i 3, i 0, i 0) i .r1'
    IntCmp $1 -1 check_error opened opened
    check_error:
        System::Call 'kernel32::GetLastError() i .r2'
        IntCmp $2 32 in_use cleanup cleanup ; only a sharing violation means "running"; any other failure isn't this check's business
    in_use:
        MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "branchDAM Agent is currently running.$\r$\n$\r$\nPlease quit it first (right-click the tray icon in the notification area and choose Quit), then click Retry." IDRETRY retry_open
        Pop $2
        Pop $1
        Pop $0
        Abort "Installation cancelled: branchDAM Agent is running."
    opened:
        System::Call 'kernel32::CloseHandle(i r1)'
    cleanup:
    Pop $2
    Pop $1
    Pop $0
FunctionEnd
!macroend
!insertmacro CheckExeNotRunning ""
!insertmacro CheckExeNotRunning "un."

; --- Installer Sections ---
Section "Install"
    Push "branchdam-agent.exe"
    Call CheckExeNotRunning
    Push "branchdam-agent-tray.exe"
    Call CheckExeNotRunning
    Push "branchdam-agent-ui.exe"
    Call CheckExeNotRunning

    SetOutPath "$INSTDIR"

    ; Binaries
    File "..\..\dist\branchdam-agent.exe"
    File "..\..\dist\branchdam-agent-tray.exe"
    File "..\..\dist\branchdam-agent-ui.exe"

    ; Ensure config directory exists
    CreateDirectory "$APPDATA\branchdam-agent"

    ; Write starter config only if no config exists (preserve user settings on upgrade)
    IfFileExists "$APPDATA\branchdam-agent\config.yaml" config_exists config_new
    config_new:
        FileOpen $0 "$APPDATA\branchdam-agent\config.yaml" w
        FileWrite $0 "# branchDAM Agent configuration$\r$\n"
        FileWrite $0 "# Configured through the tray's Settings menu.$\r$\n"
        FileWrite $0 "server:$\r$\n"
        FileWrite $0 "  baseUrl: $\"$\"$\r$\n"
        FileWrite $0 "  apiKey: $\"$\"$\r$\n"
        FileWrite $0 "$\r$\n"
        FileWrite $0 "agentId: $\"$AGENT_ID$\"$\r$\n"
        FileWrite $0 "$\r$\n"
        FileWrite $0 "pathMappings: []$\r$\n"
        FileWrite $0 "$\r$\n"
        FileWrite $0 "ingest:$\r$\n"
        FileWrite $0 "  archiveRoot: $\"$\"$\r$\n"
        FileWrite $0 "  localEditRoot: $\"$\"$\r$\n"
        FileWrite $0 "  cardRoots: []$\r$\n"
        FileClose $0
    config_exists:

    ; Create uninstaller
    WriteUninstaller "$INSTDIR\uninstall.exe"

    ; Start Menu shortcuts
    CreateDirectory "$SMPROGRAMS\${PRODUCT_NAME}"
    CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\${PRODUCT_NAME}.lnk" "$INSTDIR\branchdam-agent-tray.exe" "tray"
    CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\Open branchDAM.lnk" "$INSTDIR\branchdam-agent-ui.exe"
    CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\Uninstall.lnk" "$INSTDIR\uninstall.exe"

    ; branchdam:// protocol handler, so the server's "Pair with local agent"
    ; button opens the agent (per-user, no admin). The GUI-subsystem tray
    ; binary avoids a console flash; a bare URL argument is routed to
    ; `pair -deeplink` (cmd/branchdam-agent effectiveLaunchArgs), which asks
    ; for confirmation before writing any config.
    WriteRegStr HKCU "Software\Classes\branchdam" "" "URL:branchDAM pairing"
    WriteRegStr HKCU "Software\Classes\branchdam" "URL Protocol" ""
    WriteRegStr HKCU "Software\Classes\branchdam\DefaultIcon" "" "$INSTDIR\branchdam-agent.exe,0"
    WriteRegStr HKCU "Software\Classes\branchdam\shell\open\command" "" '"$INSTDIR\branchdam-agent-tray.exe" "%1"'

    ; Add/Remove Programs entry
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "DisplayName" "${PRODUCT_NAME}"
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "UninstallString" '"$INSTDIR\uninstall.exe"'
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "InstallLocation" "$INSTDIR"
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "DisplayIcon" "$INSTDIR\branchdam-agent.exe"
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "Publisher" "${PRODUCT_PUBLISHER}"
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "URLInfoAbout" "${PRODUCT_WEB_SITE}"
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "DisplayVersion" "${VERSION}"
    WriteRegDWORD HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "NoModify" 1
    WriteRegDWORD HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "NoRepair" 1

    ; Get installed size. $0/$1/$2 are scratch registers here -- the agent
    ; ID lives in the named $AGENT_ID var (untouched by this call), so
    ; there's nothing left in $0/$1 worth preserving at this point.
    ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
    IntFmt $0 "0x%08X" $0
    WriteRegDWORD HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "EstimatedSize" "$0"
SectionEnd

; --- Uninstaller ---
Section "Uninstall"
    Push "branchdam-agent.exe"
    Call un.CheckExeNotRunning
    Push "branchdam-agent-tray.exe"
    Call un.CheckExeNotRunning
    Push "branchdam-agent-ui.exe"
    Call un.CheckExeNotRunning

    ; Remove the branchdam:// protocol handler
    DeleteRegKey HKCU "Software\Classes\branchdam"

    ; Remove Run registry key
    DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "BranchDAMAgent"

    ; Remove files
    Delete "$INSTDIR\branchdam-agent.exe"
    Delete "$INSTDIR\branchdam-agent-tray.exe"
    Delete "$INSTDIR\branchdam-agent-ui.exe"
    Delete "$INSTDIR\uninstall.exe"

    ; internal/selfupdate.Apply writes a "<target>.previous" backup and (next
    ; to whichever binary Apply treated as Primary -- either .exe, depending
    ; on how the tray was launched) a "<primary>.previous.version" sidecar
    ; next to the binaries on every successful self-update, for the tray's
    ; "Roll back to vX" menu item (see docs/platform-support.md's Rollback
    ; section). Neither is covered by the plain Delete calls above, so
    ; without this the non-recursive RMDir "$INSTDIR" below silently leaves
    ; $INSTDIR non-empty (and thus not actually removed) after every install
    ; that ever self-updated. An uninstall is a deliberate "remove
    ; everything" action, unlike config.yaml below, which an uninstall
    ; preserves on purpose.
    Delete "$INSTDIR\branchdam-agent.exe.previous"
    Delete "$INSTDIR\branchdam-agent-tray.exe.previous"
    Delete "$INSTDIR\branchdam-agent-ui.exe.previous"
    Delete "$INSTDIR\branchdam-agent.exe.previous.version"
    Delete "$INSTDIR\branchdam-agent-tray.exe.previous.version"
    Delete "$INSTDIR\branchdam-agent-ui.exe.previous.version"
    RMDir "$INSTDIR"

    ; config.yaml is deliberately left in place -- an uninstall/reinstall
    ; cycle (a common Windows "repair" path) must not silently discard the
    ; operator's server URL, API key, and roots, the same guarantee the
    ; install-side IfFileExists check already gives an upgrade. RMDir
    ; (non-recursive) below is a no-op while config.yaml still exists;
    ; it only removes the directory once it's actually empty.
    RMDir "$APPDATA\branchdam-agent"

    ; Remove Start Menu shortcuts
    Delete "$SMPROGRAMS\${PRODUCT_NAME}\${PRODUCT_NAME}.lnk"
    Delete "$SMPROGRAMS\${PRODUCT_NAME}\Open branchDAM.lnk"
    Delete "$SMPROGRAMS\${PRODUCT_NAME}\Uninstall.lnk"
    RMDir "$SMPROGRAMS\${PRODUCT_NAME}"

    ; Remove Add/Remove Programs entry
    DeleteRegKey HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}"
SectionEnd
