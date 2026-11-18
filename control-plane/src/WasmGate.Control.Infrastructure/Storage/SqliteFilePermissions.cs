using Microsoft.Extensions.Logging;

namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>
/// Best-effort owner-only permissions for SQLite files on Unix.
/// </summary>
/// <remarks>
/// SQLite creates WAL and shared-memory files next to the database. They carry
/// database pages, so they should not be readable by other local users. Windows
/// inherits the per-user temporary/data directory ACL, so only Unix modes are
/// adjusted here.
/// </remarks>
internal static class SqliteFilePermissions
{
    private const UnixFileMode OwnerReadWrite =
        UnixFileMode.UserRead | UnixFileMode.UserWrite;

    /// <summary>Restricts an existing file to its owner on Unix.</summary>
    /// <param name="path">File to restrict; missing files are ignored.</param>
    /// <param name="logger">Receives a warning when the mode cannot be changed.</param>
    public static void RestrictToOwner(string path, ILogger logger)
    {
        if (OperatingSystem.IsWindows() || !File.Exists(path))
        {
            return;
        }

        try
        {
            File.SetUnixFileMode(path, OwnerReadWrite);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            // Do not fail startup on a filesystem that refuses mode changes,
            // but leave an explicit trace for the operator.
            logger.LogWarning(
                ex,
                "could not restrict permissions on SQLite file {Path}",
                path);
        }
    }
}
