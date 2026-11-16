using Dapper;
using Npgsql;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.Infrastructure.Repositories.Convert;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// PostgreSQL implementation of the environment catalog.
/// </summary>
/// <remarks>
/// The table is the authoritative list, but <see cref="ListAsync"/> also unions
/// the names referenced by configurations, releases, deployments and agents. An
/// upgraded installation whose rows predate the <c>environments</c> table
/// therefore still shows a complete picker even if the backfill was skipped.
/// </remarks>
public sealed class PostgresEnvironmentRepository(NpgsqlDataSource dataSource) : IEnvironmentRepository
{
    /// <summary>Connection pool for the control-plane database.</summary>
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public async Task<IReadOnlyList<string>> ListAsync(CancellationToken cancellationToken)
    {
        const string sql = """
            SELECT name FROM environments
            UNION
            SELECT environment AS name FROM config_versions
            UNION
            SELECT environment AS name FROM releases
            UNION
            SELECT environment AS name FROM deployments
            UNION
            SELECT environment AS name FROM agents
            ORDER BY name
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var rows = await connection.QueryAsync<EnvironmentNameRow>(sql);
        return rows.Select(row => row.Name).ToList();
    }

    /// <inheritdoc />
    public async Task<bool> EnsureAsync(
        string name,
        string actor,
        CancellationToken cancellationToken)
    {
        const string sql = """
            INSERT INTO environments (name, created_by, created_at)
            VALUES (@name, @createdBy, @createdAt)
            ON CONFLICT (name) DO NOTHING
            """;

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        var affectedRows = await connection.ExecuteAsync(sql, new
        {
            name,
            createdBy = actor,
            createdAt = PostgresTimestamps.ToParameter(DateTimeOffset.UtcNow),
        });
        return affectedRows == 1;
    }

    /// <inheritdoc />
    public async Task DeleteAsync(string name, CancellationToken cancellationToken)
    {
        if (string.Equals(name, EnvironmentNaming.Default, StringComparison.OrdinalIgnoreCase))
        {
            throw new InvalidOperationException(
                $"The default environment '{EnvironmentNaming.Default}' cannot be deleted.");
        }

        await using var connection = await DataSource.OpenConnectionAsync(cancellationToken);
        await using var transaction = await connection.BeginTransactionAsync(cancellationToken);

        // Serialise with enrollment and other writers that hold the environment
        // session lock. The same key is used by PostgresEnvironmentOperationLock.
        await connection.ExecuteAsync(
            "SELECT pg_advisory_xact_lock(hashtextextended(@key, 0))",
            new { key = PostgresLockKeys.Environment(name) },
            transaction: transaction);

        var exists = await connection.ExecuteScalarAsync<bool>(
            "SELECT EXISTS (SELECT 1 FROM environments WHERE name = @name)",
            new { name },
            transaction: transaction);
        if (!exists)
        {
            throw new KeyNotFoundException($"Environment '{name}' does not exist.");
        }

        var agentCount = await connection.ExecuteScalarAsync<int>(
            "SELECT count(*)::int FROM agents WHERE environment = @name",
            new { name },
            transaction: transaction);
        if (agentCount > 0)
        {
            throw new InvalidOperationException(
                $"Environment '{name}' still has {agentCount} agent(s). " +
                "Move them to another environment first.");
        }

        var runningCount = await connection.ExecuteScalarAsync<int>(
            "SELECT count(*)::int FROM deployments " +
            "WHERE environment = @name AND status IN ('Pending', 'Running')",
            new { name },
            transaction: transaction);
        if (runningCount > 0)
        {
            throw new InvalidOperationException(
                $"Environment '{name}' has {runningCount} running deployment(s). " +
                "Wait for them to finish first.");
        }

        // Deployments reference releases, and releases reference configuration
        // versions, so delete in dependency order. Waves and release artifacts
        // disappear with their parent rows through ON DELETE CASCADE.
        try
        {
            await connection.ExecuteAsync(
                "DELETE FROM deployments WHERE environment = @name",
                new { name },
                transaction: transaction);
            await connection.ExecuteAsync(
                "DELETE FROM releases WHERE environment = @name",
                new { name },
                transaction: transaction);
            await connection.ExecuteAsync(
                "DELETE FROM config_versions WHERE environment = @name",
                new { name },
                transaction: transaction);
            await connection.ExecuteAsync(
                "DELETE FROM enrollment_tokens WHERE environment = @name",
                new { name },
                transaction: transaction);
            await connection.ExecuteAsync(
                "DELETE FROM environments WHERE name = @name",
                new { name },
                transaction: transaction);

            await transaction.CommitAsync(cancellationToken);
        }
        catch (PostgresException ex) when (ex.SqlState == PostgresErrorCodes.ForeignKeyViolation)
        {
            // Normally impossible: the API scopes releases and deployments to one
            // environment. A manual SQL edit can create a cross-environment
            // reference; report it instead of returning an opaque 500.
            throw new InvalidOperationException(
                $"Environment '{name}' is still referenced by data outside the environment. " +
                "Delete that data first.", ex);
        }
    }
}

/// <summary>Row shape of the environment catalog query.</summary>
internal sealed class EnvironmentNameRow
{
    public string Name { get; set; } = string.Empty;
}
