namespace WasmGate.Control.Infrastructure.Storage;

/// <summary>
/// Serializes SQLite backups so one admin request cannot start several
/// <c>VACUUM INTO</c> operations at once.
/// </summary>
/// <remarks>
/// A backup is a CPU- and disk-intensive operation. Without a gate, a caller
/// holding the admin token could exhaust the control-plane volume by issuing
/// parallel downloads. The lease is released only after the response stream is
/// disposed, so the next backup starts when the previous snapshot has been
/// fully served or abandoned.
/// </remarks>
public sealed class SqliteBackupCoordinator
{
    private readonly SemaphoreSlim _gate = new(initialCount: 1, maxCount: 1);

    /// <summary>Tries to acquire the single backup slot without waiting.</summary>
    /// <returns>A lease when the slot is free; otherwise null.</returns>
    public IDisposable? TryAcquire() =>
        _gate.Wait(0) ? new Lease(_gate) : null;

    private sealed class Lease(SemaphoreSlim gate) : IDisposable
    {
        private int _released;

        public void Dispose()
        {
            if (Interlocked.Exchange(ref _released, 1) == 0)
            {
                gate.Release();
            }
        }
    }
}
