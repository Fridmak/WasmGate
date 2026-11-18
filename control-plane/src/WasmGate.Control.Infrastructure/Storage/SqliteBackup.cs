using Microsoft.Data.Sqlite;

namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>
/// Creates a consistent copy of the SQLite database while the control plane is
/// running.
/// </summary>
/// <remarks>
/// <c>VACUUM INTO</c> produces a compact snapshot without stopping the service.
/// It is the supported alternative to copying a live WAL database by hand.
/// </remarks>
public static class SqliteBackup
{
    /// <summary>Writes a snapshot of <paramref name="sourcePath"/> to <paramref name="destinationPath"/>.</summary>
    /// <param name="sourcePath">Live SQLite database path.</param>
    /// <param name="destinationPath">Destination file; must not exist.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    public static async Task BackupAsync(
        string sourcePath,
        string destinationPath,
        CancellationToken cancellationToken)
    {
        var source = Path.GetFullPath(sourcePath);
        if (!File.Exists(source))
        {
            throw new FileNotFoundException(
                $"SQLite database '{source}' does not exist.",
                source);
        }

        var destination = Path.GetFullPath(destinationPath);
        if (File.Exists(destination))
        {
            throw new IOException($"Backup destination '{destination}' already exists.");
        }

        var directory = Path.GetDirectoryName(destination);
        if (!string.IsNullOrEmpty(directory))
        {
            Directory.CreateDirectory(directory);
        }

        var connectionString = new SqliteConnectionStringBuilder
        {
            DataSource = source,
            Mode = SqliteOpenMode.ReadWrite,
            Pooling = false,
        }.ToString();

        await using var connection = new SqliteConnection(connectionString);
        await connection.OpenAsync(cancellationToken);
        await using var command = connection.CreateCommand();
        command.CommandText = "VACUUM INTO @path";
        command.Parameters.AddWithValue("@path", destination);
        await command.ExecuteNonQueryAsync(cancellationToken);

        if (!OperatingSystem.IsWindows())
        {
            File.SetUnixFileMode(
                destination,
                UnixFileMode.UserRead | UnixFileMode.UserWrite);
        }
    }
}
