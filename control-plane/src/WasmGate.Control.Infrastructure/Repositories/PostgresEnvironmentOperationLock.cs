using Npgsql;
using WasmGate.Control.Core.Abstractions.Repositories;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// PostgreSQL session-advisory-lock implementation of the environment gate.
/// </summary>
/// <remarks>
/// The lock lives on a dedicated connection, not in a short transaction, because
/// an enrollment spans several repository calls. PostgreSQL releases session
/// locks automatically if the connection drops, so a crashed process cannot
/// leave an environment permanently locked.
/// </remarks>
public sealed class PostgresEnvironmentOperationLock(NpgsqlDataSource dataSource) : IEnvironmentOperationLock
{
    /// <inheritdoc />
    public async Task<IAsyncDisposable> AcquireAsync(
        string environment,
        CancellationToken cancellationToken)
    {
        var connection = await dataSource.OpenConnectionAsync(cancellationToken);
        var key = PostgresLockKeys.Environment(environment);

        try
        {
            await using var command = new NpgsqlCommand(
                "SELECT pg_advisory_lock(hashtextextended(@key, 0))",
                connection);
            command.Parameters.AddWithValue("key", key);
            await command.ExecuteNonQueryAsync(cancellationToken);
            return new Handle(connection, key);
        }
        catch
        {
            await connection.DisposeAsync();
            throw;
        }
    }

    /// <summary>Releases the advisory lock and closes its dedicated connection.</summary>
    private sealed class Handle(NpgsqlConnection connection, string key) : IAsyncDisposable
    {
        /// <inheritdoc />
        public async ValueTask DisposeAsync()
        {
            try
            {
                await using var command = new NpgsqlCommand(
                    "SELECT pg_advisory_unlock(hashtextextended(@key, 0))",
                    connection);
                command.Parameters.AddWithValue("key", key);
                await command.ExecuteNonQueryAsync();
            }
            catch
            {
                // Closing the connection releases all session locks anyway.
            }
            finally
            {
                await connection.DisposeAsync();
            }
        }
    }
}
