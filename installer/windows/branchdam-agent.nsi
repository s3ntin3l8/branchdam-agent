; branchDAM Agent NSIS Installer
; Minimal installer: extract files, create shortcuts, write starter config.
; Requires NSIS 3.x (https://nsis.sourceforge.io)

!include "MUI2.nsh"
!include "FileFunc.nsh"
!include "WinMessages.nsh"

; --- Version (populated by CI) ---
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

; --- Interface ---
!define MUI_ABORTWARNING
!define MUI_ICON "..\..\resources\icons\icon.ico"
!define MUI_UNICON "..\..\resources\icons\icon.ico"
!define MUI_HEADERIMAGE
!define MUI_HEADERIMAGE_BITMAP "..\..\resources\icons\installer-header.bmp"
!define MUI_WELCOMEFINISHPAGE_BITMAP "..\..\resources\icons\installer-sidebar.bmp"
!define MUI_FINISHPAGE_RUN "$INSTDIR\branchdam-agent-tray.exe"
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

; --- Installer Init ---
Function .onInit
    ; Generate default agent ID from computer name
    System::Call 'kernel32::GetComputerName(t .r0, *i 256) i .r1'
    ${If} $1 == 0
        StrCpy $0 "workstation-01"
    ${EndIf}
FunctionEnd

; --- Installer Sections ---
Section "Install"
    SetOutPath "$INSTDIR"

    ; Binaries
    File "..\..\dist\branchdam-agent.exe"
    File "..\..\dist\branchdam-agent-tray.exe"

    ; Ensure config directory exists
    CreateDirectory "$APPDATA\branchdam-agent"

    ; Write starter config only if no config exists (preserve user settings on upgrade)
    IfFileExists "$APPDATA\branchdam-agent\config.yaml" config_exists config_new
    config_new:
        FileOpen $0 "$APPDATA\branchdam-agent\config.yaml" w
        FileWrite $0 "# branchDAM Agent configuration$\r$\n"
        FileWrite $0 "# Configured through the tray's Settings menu.$\r$\n"
        FileWrite $0 "server:$\r$\n"
        FileWrite $0 "  baseUrl: """"$\r$\n"
        FileWrite $0 "  apiKey: """"$\r$\n"
        FileWrite $0 "$\r$\n"
        FileWrite $0 "agentId: ""$0""$\r$\n"
        FileWrite $0 "$\r$\n"
        FileWrite $0 "pathMappings: []$\r$\n"
        FileWrite $0 "$\r$\n"
        FileWrite $0 "ingest:$\r$\n"
        FileWrite $0 "  archiveRoot: """"$\r$\n"
        FileWrite $0 "  localEditRoot: """"$\r$\n"
        FileWrite $0 "  cardRoots: []$\r$\n"
        FileClose $0
    config_exists:

    ; Create uninstaller
    WriteUninstaller "$INSTDIR\uninstall.exe"

    ; Start Menu shortcuts
    CreateDirectory "$SMPROGRAMS\${PRODUCT_NAME}"
    CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\${PRODUCT_NAME}.lnk" "$INSTDIR\branchdam-agent-tray.exe"
    CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\Uninstall.lnk" "$INSTDIR\uninstall.exe"

    ; Add/Remove Programs entry
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "DisplayName" "${PRODUCT_NAME}"
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "UninstallString" '"$INSTDIR\uninstall.exe"'
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "InstallLocation" "$INSTDIR"
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "DisplayIcon" "$INSTDIR\branchdam-agent.exe"
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "Publisher" "${PRODUCT_PUBLISHER}"
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "URLInfoAbout" "${PRODUCT_WEB_SITE}"
    WriteRegDWORD HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "NoModify" 1
    WriteRegDWORD HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "NoRepair" 1

    ; Get installed size
    ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
    IntFmt $0 "0x%08X" $0
    WriteRegDWORD HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "EstimatedSize" "$0"
SectionEnd

Section "Start at login"
    ; Write Run registry key for auto-start
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "BranchDAMAgent" '"$INSTDIR\branchdam-agent-tray.exe"'
SectionEnd

; --- Uninstaller ---
Section "Uninstall"
    ; Remove Run registry key
    DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "BranchDAMAgent"

    ; Remove files
    Delete "$INSTDIR\branchdam-agent.exe"
    Delete "$INSTDIR\branchdam-agent-tray.exe"
    Delete "$INSTDIR\uninstall.exe"
    RMDir "$INSTDIR"

    ; Remove config file and directory
    Delete "$APPDATA\branchdam-agent\config.yaml"
    RMDir "$APPDATA\branchdam-agent"

    ; Remove Start Menu shortcuts
    Delete "$SMPROGRAMS\${PRODUCT_NAME}\${PRODUCT_NAME}.lnk"
    Delete "$SMPROGRAMS\${PRODUCT_NAME}\Uninstall.lnk"
    RMDir "$SMPROGRAMS\${PRODUCT_NAME}"

    ; Remove Add/Remove Programs entry
    DeleteRegKey HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}"
SectionEnd
