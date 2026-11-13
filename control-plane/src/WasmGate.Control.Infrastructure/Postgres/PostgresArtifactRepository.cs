using Npgsql;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Postgres;

/// <summary>PostgreSQL implementation of the artifact metadata store.</summary>
public sealed class PostgresArtifactRepository(NpgsqlDataSource dataSource) : IArtifactRepository
{
    private NpgsqlDataSource DataSource { get; } = dataSource;

    /// <inheritdoc />
    public Task AddAsync(Artifact artifact, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: INSERT INTO artifacts (...) VALUES (...)");

    /// <inheritdoc />
    public Task<Artifact?> FindAsync(string id, CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: SELECT ... FROM artifacts WHERE id = @id");

    /// <inheritdoc />
    public Task<IReadOnlyList<Artifact>> ListAsync(CancellationToken cancellationToken) =>
        throw new NotImplementedException("TODO: SELECT ... FROM artifacts ORDER BY created_at DESC");
}
