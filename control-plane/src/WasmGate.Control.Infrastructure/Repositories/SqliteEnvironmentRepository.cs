using System.Data;
using Dapper;
using Microsoft.Data.Sqlite;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.Infrastructure.Storage;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// SQLite implementation of the environment catalog.
/// </summary>
/// <remarks>
/// The table is authoritative, but <see cref="ListAsync"/> also unions names
/// referenced by configurations, releases, deployments and agents so an upgraded
/// installation still shows a complete picker.
/// </remarks>
public sealed class SqliteEnvironmentRepository(SqliteConnectionFactory connections) : IEnvironmentRepository
{
    private SqliteConnectionFactory Connections { get; } = connections;

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

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var rows = await connection.QueryAsync<SqliteEnvironmentNameRow>(sql);
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

        await using var connection = await Connections.OpenAsync(cancellationToken);
        var affectedRows = await connection.ExecuteAsync(sql, new
        {
            name,
            createdBy = actor,
            createdAt = SqliteTimestamps.ToParameter(DateTimeOffset.UtcNow),
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

        await using var connection = await Connections.OpenAsync(cancellationToken);
        await using var transaction = connection.BeginTransaction(
            IsolationLevel.Serializable,
            deferred: false);

        var exists = await connection.ExecuteScalarAsync<bool>(
            "SELECT EXISTS (SELECT 1 FROM environments WHERE name = @name)",
            new { name },
            transaction);
        if (!exists)
        {
            throw new KeyNotFoundException($"Environment '{name}' does not exist.");
        }

        var agentCount = await connection.ExecuteScalarAsync<long>(
            "SELECT COUNT(*) FROM agents WHERE environment = @name",
            new { name },
            transaction);
        if (agentCount > 0)
        {
            throw new InvalidOperationException(
                $"Environment '{name}' still has {agentCount} agent(s). " +
                "Move them to another environment first.");
        }

        var runningCount = await connection.ExecuteScalarAsync<long>(
            "SELECT COUNT(*) FROM deployments " +
            "WHERE environment = @name AND status IN ('Pending', 'Running')",
            new { name },
            transaction);
        if (runningCount > 0)
        {
            throw new InvalidOperationException(
                $"Environment '{name}' has {runningCount} running deployment(s). " +
                "Wait for them to finish first.");
        }

        try
        {
            await connection.ExecuteAsync(
                "DELETE FROM deployments WHERE environment = @name",
                new { name },
                transaction);
            await connection.ExecuteAsync(
                "DELETE FROM releases WHERE environment = @name",
                new { name },
                transaction);
            await connection.ExecuteAsync(
                "DELETE FROM config_versions WHERE environment = @name",
                new { name },
                transaction);
            await connection.ExecuteAsync(
                "DELETE FROM enrollment_tokens WHERE environment = @name",
                new { name },
                transaction);
            await connection.ExecuteAsync(
                "DELETE FROM environments WHERE name = @name",
                new { name },
                transaction);

            await transaction.CommitAsync(cancellationToken);
        }
        catch (SqliteException ex) when (SqliteErrors.IsForeignKeyViolation(ex))
        {
            throw new InvalidOperationException(
                $"Environment '{name}' is still referenced by data outside the environment. " +
                "Delete that data first.", ex);
        }
    }
}

/// <summary>Row shape of the environment catalog union query.</summary>
internal sealed class SqliteEnvironmentNameRow
{
    public string Name { get; set; } = string.Empty;
}
