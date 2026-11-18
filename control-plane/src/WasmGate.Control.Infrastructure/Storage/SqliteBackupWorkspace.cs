using Microsoft.Extensions.Logging;

namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>
/// A private, short-lived directory that holds one SQLite snapshot.
/// </summary>
/// <remarks>
/// The directory is created below the configured backup root (the system
/// temporary directory by default) with a random name and owner-only
/// permissions on Unix. Keeping the snapshot out of a shared directory avoids
/// exposing a copy of the control-plane database to other local users and
/// removes a symlink race between the destination check and
/// <c>VACUUM INTO</c>.
/// </remarks>
public sealed class SqliteBackupWorkspace : IDisposable
{
    private const string Prefix = "wasmgate-control-backup-";

    private int _disposed;

    private SqliteBackupWorkspace(string directoryPath)
    {
        DirectoryPath = directoryPath;
    }

    /// <summary>Absolute path of the private workspace directory.</summary>
    public string DirectoryPath { get; }

    /// <summary>Creates a private directory for one snapshot.</summary>
    /// <param name="root">
    /// Optional parent directory. Null or empty uses the system temporary
    /// directory.
    /// </param>
    /// <returns>A workspace that removes its directory when disposed.</returns>
    public static SqliteBackupWorkspace Create(string? root)
    {
        var baseDirectory = string.IsNullOrWhiteSpace(root)
            ? Path.GetTempPath()
            : Path.GetFullPath(root);

        Directory.CreateDirectory(baseDirectory);

        var directory = Path.Combine(
            baseDirectory,
            Prefix + Guid.NewGuid().ToString("N"));
        Directory.CreateDirectory(directory);

        if (!OperatingSystem.IsWindows())
        {
            File.SetUnixFileMode(
                directory,
                UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute);
        }

        return new SqliteBackupWorkspace(directory);
    }

    /// <summary>Returns a file path inside the workspace.</summary>
    /// <param name="fileName">Relative file name; must not contain separators.</param>
    /// <returns>The combined absolute path.</returns>
    public string PathFor(string fileName) => Path.Combine(DirectoryPath, fileName);

    /// <summary>Removes workspaces left behind by a crashed process.</summary>
    /// <param name="root">Configured backup root, or null for the system temporary directory.</param>
    /// <param name="minimumAge">Only directories older than this are removed.</param>
    /// <param name="logger">Receives cleanup diagnostics.</param>
    /// <remarks>
    /// A process kill cannot run normal cleanup, so the next startup sweeps
    /// abandoned snapshots. The age guard leaves an active backup of another
    /// process alone.
    /// </remarks>
    public static void CleanStale(
        string? root,
        TimeSpan minimumAge,
        ILogger logger)
    {
        var baseDirectory = string.IsNullOrWhiteSpace(root)
            ? Path.GetTempPath()
            : Path.GetFullPath(root);

        if (!Directory.Exists(baseDirectory))
        {
            return;
        }

        var cutoff = DateTime.UtcNow - minimumAge;
        foreach (var directory in Directory.EnumerateDirectories(
                     baseDirectory,
                     Prefix + "*"))
        {
            try
            {
                if (Directory.GetLastWriteTimeUtc(directory) > cutoff)
                {
                    continue;
                }

                Directory.Delete(directory, recursive: true);
                logger.LogInformation(
                    "removed stale SQLite backup workspace {Directory}",
                    directory);
            }
            catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
            {
                logger.LogWarning(
                    ex,
                    "could not remove stale SQLite backup workspace {Directory}",
                    directory);
            }
        }
    }

    /// <summary>Removes the workspace and every file in it.</summary>
    public void Dispose()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0)
        {
            return;
        }

        try
        {
            Directory.Delete(DirectoryPath, recursive: true);
        }
        catch (IOException)
        {
            // A failed cleanup must not hide the original backup outcome.
        }
        catch (UnauthorizedAccessException)
        {
            // The directory may be held by an antivirus scanner or another
            // process; it is private and contains no live connection.
        }
    }
}
