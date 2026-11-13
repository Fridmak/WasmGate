using System.Collections.Concurrent;
using System.Diagnostics.CodeAnalysis;
using WasmGate.Control.Core.Abstractions;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>
/// In-memory registry of connected agents. Enough for one CP process.
/// </summary>
/// <remarks>
/// Reads are lock-free. Register and unregister are serialized by a small lock
/// so a displaced session can be returned and a stale unregister cannot remove
/// the newer session.
/// </remarks>
public sealed class InMemoryAgentSessionRegistry : IAgentSessionRegistry
{
    private readonly ConcurrentDictionary<Guid, IAgentSession> _sessions = new();
    private readonly Lock _writeGate = new();

    /// <inheritdoc />
    public int Count => _sessions.Count;

    /// <inheritdoc />
    public IAgentSession? Register(Guid agentId, IAgentSession session)
    {
        ArgumentNullException.ThrowIfNull(session);

        lock (_writeGate)
        {
            _sessions.TryGetValue(agentId, out var previous);
            _sessions[agentId] = session;
            return ReferenceEquals(previous, session) ? null : previous;
        }
    }

    /// <inheritdoc />
    public void Unregister(Guid agentId, IAgentSession session)
    {
        lock (_writeGate)
        {
            if (_sessions.TryGetValue(agentId, out var current) && ReferenceEquals(current, session))
            {
                _sessions.TryRemove(agentId, out _);
            }
        }
    }

    /// <inheritdoc />
    public bool TryGet(Guid agentId, [NotNullWhen(true)] out IAgentSession? session) =>
        _sessions.TryGetValue(agentId, out session);
}
