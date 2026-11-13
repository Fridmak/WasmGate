using System.Collections.Concurrent;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>In-memory agent inventory. Replace with PostgreSQL for production.</summary>
public sealed class InMemoryAgentRepository : IAgentRepository
{
    private readonly ConcurrentDictionary<Guid, Agent> _agents = new();
    private readonly ConcurrentDictionary<string, Guid> _byName = new(StringComparer.Ordinal);

    /// <inheritdoc />
    public Task<IReadOnlyList<Agent>> ListAsync(CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        return Task.FromResult<IReadOnlyList<Agent>>(_agents.Values.OrderBy(a => a.Name).ToList());
    }

    /// <inheritdoc />
    public Task<Agent?> FindAsync(Guid id, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        return Task.FromResult(_agents.GetValueOrDefault(id));
    }

    /// <inheritdoc />
    public Task<Agent?> FindByNameAsync(string name, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        return Task.FromResult(_byName.TryGetValue(name, out var id) ? _agents.GetValueOrDefault(id) : null);
    }

    /// <inheritdoc />
    public Task UpsertAsync(Agent agent, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        _agents[agent.Id] = agent;
        _byName[agent.Name] = agent.Id;
        return Task.CompletedTask;
    }

    /// <inheritdoc />
    public Task TouchAsync(
        Guid id,
        DateTimeOffset seenAt,
        string? activeReleaseId,
        string? activeConfigSha256,
        CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        if (_agents.TryGetValue(id, out var agent))
        {
            agent.LastSeenAt = seenAt;
            agent.ActiveReleaseId = activeReleaseId;
            agent.ActiveConfigSha256 = activeConfigSha256;
            agent.Status = AgentStatus.Online;
        }

        return Task.CompletedTask;
    }
}
