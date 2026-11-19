using System.Collections.Concurrent;
using System.Diagnostics.CodeAnalysis;
using WasmGate.Control.Core.Abstractions.Agents;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>
/// Process-local registry of connected agents: one entry per agent identity,
/// pointing at its live control stream.
/// </summary>
/// <remarks>
/// <para>
/// The dictionary lives in memory because a session is an open transport, not
/// data; the durable facts about an agent are stored by the agent repository.
/// The registry therefore serves exactly one control-plane process: a second
/// instance would not see the sessions of the first.
/// </para>
/// <para>
/// Reads are lock-free. Register and unregister share a small lock: register must
/// return the displaced session so the caller can take it over, and unregister
/// must not remove a newer session that the same agent has already registered.
/// </para>
/// </remarks>
public sealed class InMemoryAgentSessionRegistry : IAgentSessionRegistry
{
    private readonly ConcurrentDictionary<Guid, IAgentSession> _sessions = new();
    private readonly Lock _writeLock = new();

    /// <inheritdoc />
    public int Count => _sessions.Count;

    /// <inheritdoc />
    public IAgentSession? Register(Guid agentId, IAgentSession session)
    {
        ArgumentNullException.ThrowIfNull(session);

        lock (_writeLock)
        {
            _sessions.TryGetValue(agentId, out var previous);
            _sessions[agentId] = session;
            return ReferenceEquals(previous, session) ? null : previous;
        }
    }

    /// <inheritdoc />
    public void Unregister(Guid agentId, IAgentSession session)
    {
        lock (_writeLock)
        {
            if (_sessions.TryGetValue(agentId, out var current) && ReferenceEquals(current, session))
            {
                _sessions.TryRemove(agentId, out _);
            }
        }
    }

    /// <inheritdoc />
    public bool TryRestore(
        Guid agentId,
        IAgentSession failedSession,
        IAgentSession displacedSession)
    {
        ArgumentNullException.ThrowIfNull(failedSession);
        ArgumentNullException.ThrowIfNull(displacedSession);

        lock (_writeLock)
        {
            if (!_sessions.TryGetValue(agentId, out var current) ||
                !ReferenceEquals(current, failedSession))
            {
                return false;
            }

            _sessions[agentId] = displacedSession;
            return true;
        }
    }

    /// <inheritdoc />
    public bool TryGet(Guid agentId, [NotNullWhen(true)] out IAgentSession? session) =>
        _sessions.TryGetValue(agentId, out session);
}
