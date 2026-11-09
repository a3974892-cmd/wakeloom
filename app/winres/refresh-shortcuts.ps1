$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
$options = $env:WAKELOOM_ICON_OPTIONS | ConvertFrom-Json
Add-Type -TypeDefinition @'
using System.Runtime.InteropServices;
using System.Text;
public static class WakeloomIconPaths {
    [DllImport("kernel32.dll", CharSet=CharSet.Unicode)]
    public static extern uint GetLongPathName(string path, StringBuilder output, uint size);
}
'@
function Full-IconPath([string]$path) {
    $full = [IO.Path]::GetFullPath([Environment]::ExpandEnvironmentVariables($path))
    $buffer = New-Object Text.StringBuilder 32768
    $size = [WakeloomIconPaths]::GetLongPathName($full, $buffer, 32768)
    if ($size -gt 0 -and $size -lt 32768) { return $buffer.ToString() }
    return $full
}
$exePath = Full-IconPath $options.exe
$iconPath = (Full-IconPath $options.icon) + ',0'
$folders = $options.folders
if ($null -eq $folders) {
    $folders = @([Environment]::GetFolderPath('DesktopDirectory'), [Environment]::GetFolderPath('CommonDesktopDirectory'))
}
$changed = New-Object 'System.Collections.Generic.List[string]'
$failed = 0
$shell = New-Object -ComObject WScript.Shell
try {
    foreach ($folder in ($folders | Select-Object -Unique)) {
        if (-not $folder -or -not (Test-Path -LiteralPath $folder -PathType Container)) { continue }
        foreach ($file in (Get-ChildItem -LiteralPath $folder -Filter '*.lnk' -File -ErrorAction SilentlyContinue)) {
            $link = $null
            try {
                $link = $shell.CreateShortcut($file.FullName)
                if (-not $link.TargetPath) { continue }
                $target = Full-IconPath $link.TargetPath
                if (-not [String]::Equals($target, $exePath, [StringComparison]::OrdinalIgnoreCase)) { continue }
                if ([String]::Equals($link.IconLocation, $iconPath, [StringComparison]::OrdinalIgnoreCase)) { continue }
                # Preserve target, arguments, working directory, name and all other properties.
                $link.IconLocation = $iconPath
                $link.Save()
                $changed.Add($file.FullName)
            } catch { $failed++ }
            finally { if ($null -ne $link) { [void][Runtime.InteropServices.Marshal]::ReleaseComObject($link) } }
        }
    }
} finally { [void][Runtime.InteropServices.Marshal]::ReleaseComObject($shell) }
@{ changed = @($changed.ToArray()); failed = $failed } | ConvertTo-Json -Compress
