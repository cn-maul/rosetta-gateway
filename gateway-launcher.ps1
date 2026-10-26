# Windows launcher process-ownership helpers.
#
# gateway.ps1 dot-sources this file. Add-Type compiles the helper in the same
# PowerShell process, so the ownership rule has one source instead of generated
# PowerShell duplicates.

Add-Type -TypeDefinition @'
using System;
using System.Diagnostics;
using System.IO;

namespace RosettaGatewayLauncher
{
    public static class ProcessOwnership
    {
        public static bool IsGatewayExecutable(
            string imagePath, string configuredExecutable)
        {
            if (String.IsNullOrWhiteSpace(imagePath) ||
                String.IsNullOrWhiteSpace(configuredExecutable))
            {
                return false;
            }
            return String.Equals(
                Path.GetFullPath(imagePath),
                Path.GetFullPath(configuredExecutable),
                StringComparison.OrdinalIgnoreCase);
        }

        public static bool IsLauncherWrapper(
            string commandLine, string configuredExecutable)
        {
            if (String.IsNullOrWhiteSpace(commandLine) ||
                String.IsNullOrWhiteSpace(configuredExecutable))
            {
                return false;
            }
            return commandLine.IndexOf(
                Path.GetFullPath(configuredExecutable),
                StringComparison.OrdinalIgnoreCase) >= 0;
        }
    }
}
'@
