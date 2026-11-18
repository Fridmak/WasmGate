namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>
/// PostgreSQL initialization is intentionally a no-op.
/// </summary>
/// <remarks>
/// PostgreSQL schemas are applied explicitly by the operator from
/// <c>control-plane/migrations</c>. Changing that behaviour would make an
/// accidental connection string mutate a production database, so the control
/// plane keeps readiness-based reporting instead.
/// </remarks>
public sealed class PostgresStorageInitializer : IStorageInitializer
{
    /// <inheritdoc />
    public Task InitializeAsync(CancellationToken cancellationToken) => Task.CompletedTask;
}
