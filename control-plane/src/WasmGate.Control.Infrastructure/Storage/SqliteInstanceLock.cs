namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>
/// Enforces one control-plane process per SQLite database file.
/// </summary>
/// <remarks>
/// The control plane already keeps live agent sessions in memory and must run as
/// a single process. SQLite adds a single writer on top of that, so a second
/// process would produce confusing <c>SQLITE_BUSY</c> errors instead of a clear
/// failure. The lock file makes the limitation explicit at startup.
/// </remarks>
public sealed class SqliteInstanceLock : IDisposable
{
    private FileStream? _handle;

    /// <summary>File path used as the process lock.</summary>
    /// <param name="databasePath">Absolute path to the SQLite database.</param>
    /// <returns>The lock path, next to the database file.</returns>
    public static string PathFor(string databasePath) => databasePath + ".lock";

    /// <summary>
    /// Acquires the process lock, or throws when another process holds it.
    /// </summary>
    /// <param name="databasePath">Absolute path to the SQLite database.</param>
    /// <exception cref="InvalidOperationException">
    /// Another process already holds the lock for this database.
    /// </exception>
    public void Acquire(string databasePath)
    {
        if (_handle is not null)
        {
            return;
        }

        var lockPath = PathFor(databasePath);
        try
        {
            _handle = new FileStream(
                lockPath,
                FileMode.OpenOrCreate,
                FileAccess.ReadWrite,
                FileShare.None,
                bufferSize: 1,
                FileOptions.None);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            throw new InvalidOperationException(
                $"SQLite database '{databasePath}' is already in use by another control-plane process. " +
                "SQLite storage supports exactly one control-plane process per database file; " +
                "SQLite storage is designed for a single control-plane process.",
                ex);
        }
    }

    /// <inheritdoc />
    public void Dispose()
    {
        _handle?.Dispose();
        _handle = null;
    }
}
