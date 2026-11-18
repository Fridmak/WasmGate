using Microsoft.Data.Sqlite;

namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>
/// Creates and opens pooled SQLite connections for the control-plane database.
/// </summary>
/// <remarks>
/// Connection setup is deliberately centralized: the connection string enables
/// foreign keys and a busy timeout, and every connection uses the same file.
/// WAL mode is persistent in the database file and is set once by
/// <see cref="SqliteSchema"/>.
/// </remarks>
public sealed class SqliteConnectionFactory
{
    /// <summary>Creates a factory for one database file.</summary>
    /// <param name="connectionString">Microsoft.Data.Sqlite connection string.</param>
    public SqliteConnectionFactory(string connectionString)
    {
        ConnectionString = connectionString;
    }

    /// <summary>Connection string used for every connection.</summary>
    public string ConnectionString { get; }

    /// <summary>Creates a closed connection.</summary>
    /// <returns>A new, unopened connection.</returns>
    public SqliteConnection Create() => new(ConnectionString);

    /// <summary>Opens a pooled connection.</summary>
    /// <param name="cancellationToken">Cancels the open.</param>
    /// <returns>An open connection owned by the caller.</returns>
    public async Task<SqliteConnection> OpenAsync(CancellationToken cancellationToken)
    {
        var connection = Create();
        try
        {
            await connection.OpenAsync(cancellationToken);
            return connection;
        }
        catch
        {
            await connection.DisposeAsync();
            throw;
        }
    }
}
