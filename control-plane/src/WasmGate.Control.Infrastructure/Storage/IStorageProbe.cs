namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>
/// Readiness check for the selected durable store.
/// </summary>
/// <remarks>
/// The probe runs a trivial query against the schema rather than only opening a
/// connection, so a process started before migrations reports not-ready.
/// </remarks>
public interface IStorageProbe
{
    /// <summary>Returns true when the store answers a schema query.</summary>
    /// <param name="cancellationToken">Cancels the probe.</param>
    /// <returns>True when the store is ready.</returns>
    Task<bool> CheckAsync(CancellationToken cancellationToken);
}
