using Npgsql;

namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>Readiness probe for the PostgreSQL database.</summary>
/// <remarks>
/// The probe keeps the previous behaviour: a query against <c>agents</c> proves
/// both connectivity and that the migrations were applied.
/// </remarks>
public sealed class PostgresStorageProbe(NpgsqlDataSource dataSource) : IStorageProbe
{
    /// <inheritdoc />
    public async Task<bool> CheckAsync(CancellationToken cancellationToken)
    {
        try
        {
            await using var connection = await dataSource.OpenConnectionAsync(cancellationToken);
            await using var command = connection.CreateCommand();
            command.CommandText = "SELECT 1 FROM agents LIMIT 1";
            command.CommandTimeout = 2;
            await command.ExecuteScalarAsync(cancellationToken);
            return true;
        }
        catch (NpgsqlException)
        {
            return false;
        }
    }
}
