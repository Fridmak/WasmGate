using System.Collections.Concurrent;
using WasmGate.Control.Core.Abstractions.Repositories;

namespace WasmGate.Control.Infrastructure.Repositories;

/// <summary>
/// SQLite implementation of the environment deletion gate.
/// </summary>
/// <remarks>
/// SQLite storage supports exactly one control-plane process, so an in-process
/// semaphore per environment serializes operations that must not race with
/// environment deletion. The key is the normalized environment name.
/// </remarks>
public sealed class SqliteEnvironmentOperationLock : IEnvironmentOperationLock
{
    private readonly ConcurrentDictionary<string, SemaphoreSlim> _locks =
        new(StringComparer.Ordinal);

    /// <inheritdoc />
    public async Task<IAsyncDisposable> AcquireAsync(
        string environment,
        CancellationToken cancellationToken)
    {
        var gate = _locks.GetOrAdd(environment, _ => new SemaphoreSlim(1, 1));
        await gate.WaitAsync(cancellationToken);
        return new Handle(gate);
    }

    private sealed class Handle(SemaphoreSlim gate) : IAsyncDisposable
    {
        private int _released;

        public ValueTask DisposeAsync()
        {
            if (Interlocked.Exchange(ref _released, 1) == 0)
            {
                gate.Release();
            }

            return ValueTask.CompletedTask;
        }
    }
}
