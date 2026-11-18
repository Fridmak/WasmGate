using Microsoft.Extensions.Logging;
using WasmGate.Control.Core.Configuration;

namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>
/// Prepares a SQLite database file for the control plane.
/// </summary>
/// <remarks>
/// The initializer creates the parent directory, takes the single-process lock,
/// enables WAL and applies the embedded schema. It runs before Kestrel starts,
/// so a broken database fails startup instead of serving an unready process.
/// </remarks>
public sealed class SqliteStorageInitializer(
    ControlPlaneOptions options,
    SqliteConnectionFactory connections,
    SqliteInstanceLock instanceLock,
    ILogger<SqliteStorageInitializer> logger)
{
    /// <summary>Creates the directory, lock, WAL mode and schema.</summary>
    /// <param name="cancellationToken">Cancels initialization.</param>
    public async Task InitializeAsync(CancellationToken cancellationToken)
    {
        if (string.IsNullOrWhiteSpace(options.SqlitePath))
        {
            throw new InvalidOperationException(
                "WASMGATE_CP_SQLITE_PATH must not be empty.");
        }

        var databasePath = Path.GetFullPath(options.SqlitePath);
        var directory = Path.GetDirectoryName(databasePath);
        if (!string.IsNullOrEmpty(directory))
        {
            // Do not tighten the parent directory: in the compose demo the data
            // plane mounts it read-only as a different UID to read the public
            // release key. Private files are protected individually.
            Directory.CreateDirectory(directory);
        }

        instanceLock.Acquire(databasePath);

        await using var connection = await connections.OpenAsync(cancellationToken);
        await SqliteSchema.EnsureAsync(connection, cancellationToken);

        if (!OperatingSystem.IsWindows())
        {
            File.SetUnixFileMode(
                databasePath,
                UnixFileMode.UserRead | UnixFileMode.UserWrite);
            File.SetUnixFileMode(
                SqliteInstanceLock.PathFor(databasePath),
                UnixFileMode.UserRead | UnixFileMode.UserWrite);
        }

        logger.LogInformation(
            "SQLite storage ready at {DatabasePath} (schema version {SchemaVersion})",
            databasePath,
            SqliteSchema.CurrentVersion);
    }
}
