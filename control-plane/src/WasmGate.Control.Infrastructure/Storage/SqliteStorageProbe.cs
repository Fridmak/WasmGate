using Microsoft.Data.Sqlite;

namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>Readiness probe for the SQLite database.</summary>
/// <remarks>
/// The probe reads from <c>agents</c>, so a file without the schema reports
/// not-ready instead of looking healthy after a simple <c>SELECT 1</c>.
/// </remarks>
public sealed class SqliteStorageProbe(SqliteConnectionFactory connections)
{
    /// <summary>Returns true when the schema answers a trivial query.</summary>
    /// <param name="cancellationToken">Cancels the probe.</param>
    /// <returns>True when the store is ready.</returns>
    public async Task<bool> CheckAsync(CancellationToken cancellationToken)
    {
        try
        {
            await using var connection = await connections.OpenAsync(cancellationToken);
            await using var command = connection.CreateCommand();
            command.CommandText = "SELECT 1 FROM agents LIMIT 1";
            command.CommandTimeout = 2;
            await command.ExecuteScalarAsync(cancellationToken);
            return true;
        }
        catch (SqliteException)
        {
            return false;
        }
    }
}
