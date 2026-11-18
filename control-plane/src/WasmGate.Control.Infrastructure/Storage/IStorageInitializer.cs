namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>
/// Prepares the selected durable store before the control plane starts serving.
/// </summary>
/// <remarks>
/// SQLite creates its directory, acquires the process lock and applies the
/// embedded schema. PostgreSQL is a no-op: its schema is applied by the
/// operator through <c>control-plane/migrations</c>, exactly as before.
/// </remarks>
public interface IStorageInitializer
{
    /// <summary>Initializes the store. Must be called once during startup.</summary>
    /// <param name="cancellationToken">Cancels initialization.</param>
    Task InitializeAsync(CancellationToken cancellationToken);
}
