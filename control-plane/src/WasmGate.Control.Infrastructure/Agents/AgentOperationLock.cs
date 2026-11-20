using WasmGate.Control.Core.Abstractions.Agents;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>
/// Process-local keyed lock for agent lifecycle operations.
/// </summary>
/// <remarks>
/// Entries are reference-counted and removed when the last waiter leaves, so a
/// fleet with churn does not leave one semaphore per historical agent forever.
/// </remarks>
public sealed class AgentOperationLock : IAgentOperationLock
{
    private readonly object _entriesGate = new();
    private readonly Dictionary<Guid, Entry> _entries = [];

    /// <summary>Number of live or waited-on entries; used by lifecycle tests.</summary>
    internal int EntryCount
    {
        get
        {
            lock (_entriesGate)
            {
                return _entries.Count;
            }
        }
    }

    /// <inheritdoc />
    public async Task<IAsyncDisposable> AcquireAsync(
        Guid agentId,
        CancellationToken cancellationToken)
    {
        Entry entry;
        lock (_entriesGate)
        {
            if (!_entries.TryGetValue(agentId, out entry!))
            {
                entry = new Entry();
                _entries.Add(agentId, entry);
            }

            entry.References++;
        }

        try
        {
            await entry.Gate.WaitAsync(cancellationToken);
            return new Lease(this, agentId, entry);
        }
        catch
        {
            ReleaseReference(agentId, entry);
            throw;
        }
    }

    private void Release(Guid agentId, Entry entry)
    {
        entry.Gate.Release();
        ReleaseReference(agentId, entry);
    }

    private void ReleaseReference(Guid agentId, Entry entry)
    {
        lock (_entriesGate)
        {
            entry.References--;
            if (entry.References == 0 &&
                _entries.TryGetValue(agentId, out var current) &&
                ReferenceEquals(current, entry))
            {
                _entries.Remove(agentId);
                entry.Gate.Dispose();
            }
        }
    }

    private sealed class Entry
    {
        public readonly SemaphoreSlim Gate = new(1, 1);

        public int References;
    }

    private sealed class Lease(
        AgentOperationLock owner,
        Guid agentId,
        Entry entry) : IAsyncDisposable
    {
        private int _released;

        public ValueTask DisposeAsync()
        {
            if (Interlocked.Exchange(ref _released, 1) == 0)
            {
                owner.Release(agentId, entry);
            }

            return ValueTask.CompletedTask;
        }
    }
}
