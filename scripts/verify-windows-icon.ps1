param(
    [Parameter(Mandatory = $true)]
    [string]$Path
)

$resolved = (Resolve-Path -LiteralPath $Path).Path
Add-Type @'
using System;
using System.Runtime.InteropServices;
public static class IconResourceProbe {
    [DllImport("kernel32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    public static extern IntPtr LoadLibraryEx(string fileName, IntPtr file, uint flags);
    [DllImport("kernel32.dll", SetLastError=true)]
    public static extern IntPtr FindResource(IntPtr module, IntPtr name, IntPtr type);
    [DllImport("kernel32.dll", SetLastError=true)]
    public static extern bool FreeLibrary(IntPtr module);
}
'@

$LOAD_LIBRARY_AS_DATAFILE = 0x00000002
$RT_GROUP_ICON = 14
$module = [IconResourceProbe]::LoadLibraryEx($resolved, [IntPtr]::Zero, $LOAD_LIBRARY_AS_DATAFILE)
if ($module -eq [IntPtr]::Zero) {
    throw "Unable to load executable resource data: $resolved"
}
try {
    $resource = [IconResourceProbe]::FindResource($module, [IntPtr]1, [IntPtr]$RT_GROUP_ICON)
    if ($resource -eq [IntPtr]::Zero) {
        throw "Windows executable has no RT_GROUP_ICON resource with ID 1: $resolved"
    }
    Write-Host "Verified Windows icon resource in $resolved"
} finally {
    [void][IconResourceProbe]::FreeLibrary($module)
}
