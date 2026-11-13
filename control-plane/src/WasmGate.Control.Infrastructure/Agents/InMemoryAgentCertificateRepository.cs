using System.Collections.Concurrent;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>In-memory agent certificate store. Replace with PostgreSQL for production.</summary>
public sealed class InMemoryAgentCertificateRepository : IAgentCertificateRepository
{
    private readonly ConcurrentDictionary<Guid, List<AgentCertificate>> _byAgent = new();

    /// <inheritdoc />
    public Task AddAsync(AgentCertificate certificate, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        _byAgent.AddOrUpdate(
            certificate.AgentId,
            _ => [certificate],
            (_, list) =>
            {
                lock (list)
                {
                    list.Add(certificate);
                }

                return list;
            });
        return Task.CompletedTask;
    }

    /// <inheritdoc />
    public Task<IReadOnlyList<AgentCertificate>> ListActiveByAgentAsync(
        Guid agentId,
        CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        if (!_byAgent.TryGetValue(agentId, out var list))
        {
            return Task.FromResult<IReadOnlyList<AgentCertificate>>([]);
        }

        lock (list)
        {
            return Task.FromResult<IReadOnlyList<AgentCertificate>>(
                list.Where(c => c.RevokedAt is null).ToList());
        }
    }

    /// <inheritdoc />
    public Task RevokeAsync(Guid agentId, string serialNumber, CancellationToken cancellationToken)
    {
        cancellationToken.ThrowIfCancellationRequested();
        if (_byAgent.TryGetValue(agentId, out var list))
        {
            lock (list)
            {
                foreach (var certificate in list)
                {
                    if (string.Equals(certificate.SerialNumber, serialNumber, StringComparison.OrdinalIgnoreCase))
                    {
                        certificate.RevokedAt = DateTimeOffset.UtcNow;
                    }
                }
            }
        }

        return Task.CompletedTask;
    }
}
