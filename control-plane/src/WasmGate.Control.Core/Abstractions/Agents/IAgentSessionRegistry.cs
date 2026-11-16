using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Core.Entities.Delivery;

namespace WasmGate.Control.Core.Abstractions.Agents;

/// <summary>
/// Registry of live agent connections: which agents are connected at this moment
/// and through which session a command can reach them.
/// </summary>
/// <remarks>
/// The registry holds no durable state. It maps an agent to an open transport, so
/// an implementation is local to one control-plane process; durable facts about
/// agents (status, last seen, active release) live in the agent repository.
/// </remarks>
public interface IAgentSessionRegistry
{
    /// <summary>Number of agents with a registered session.</summary>
    /// <remarks>A takeover replaces the session without changing the count.</remarks>
    int Count { get; }

    /// <summary>Registers a connected session, replacing any previous one for the agent.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="session">New session that can receive commands.</param>
    /// <returns>
    /// The displaced session when the agent reconnected, or null on first
    /// registration or when the same instance is registered again. The caller
    /// owns the result and should close it to complete the takeover.
    /// </returns>
    IAgentSession? Register(Guid agentId, IAgentSession session);

    /// <summary>Removes a session that ended.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="session">Session being removed; ignored when it was already replaced.</param>
    void Unregister(Guid agentId, IAgentSession session);

    /// <summary>Looks up the session of an agent.</summary>
    /// <param name="agentId">Agent identifier.</param>
    /// <param name="session">The current session, or null when the agent is not connected.</param>
    /// <returns>True when a session exists.</returns>
    bool TryGet(Guid agentId, [System.Diagnostics.CodeAnalysis.NotNullWhen(true)] out IAgentSession? session);
}

/// <summary>A live connection to one data plane that can receive control commands.</summary>
public interface IAgentSession
{
    /// <summary>Identifier of the connected agent.</summary>
    Guid AgentId { get; }

    /// <summary>Sends one command to the agent.</summary>
    /// <param name="command">Transport-neutral command to send.</param>
    /// <param name="cancellationToken">Cancels the send operation.</param>
    /// <remarks>
    /// Concurrent sends are serialized by the implementation. The call completes
    /// when the message reaches the transport, not when the agent applies it;
    /// failures are reported by the agent as an apply result.
    /// </remarks>
    ValueTask SendAsync(AgentCommand command, CancellationToken cancellationToken);

    /// <summary>Requests the session to end and cancels its transport.</summary>
    /// <param name="cancellationToken">Cancels the request.</param>
    /// <remarks>
    /// Used during takeover and on shutdown. The call is idempotent, safe after
    /// the session has already ended, and does not wait for the agent to notice.
    /// </remarks>
    ValueTask CloseAsync(CancellationToken cancellationToken);
}
