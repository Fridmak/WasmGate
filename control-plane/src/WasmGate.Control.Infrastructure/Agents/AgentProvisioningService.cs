using System.Security.Cryptography;
using System.Text;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>
/// Provisions agents, issues enrollment tokens and performs enrollment. The
/// in-memory stores make this usable for a single CP process; swap the
/// repositories for PostgreSQL when persistence is needed.
/// </summary>
public sealed class AgentProvisioningService(
    IAgentRepository agents,
    IEnrollmentTokenRepository tokens,
    IAgentCertificateRepository certificates,
    ICertificateAuthority authority,
    ControlPlaneOptions options) : IAgentProvisioningService
{
    /// <inheritdoc />
    public async Task<AgentProvisioning> CreateAgentAsync(
        string name,
        IReadOnlyDictionary<string, string> labels,
        string actor,
        CancellationToken cancellationToken)
    {
        if (string.IsNullOrWhiteSpace(name))
        {
            throw new EnrollmentException(EnrollmentFailure.CsrInvalid, "Agent name is required.");
        }

        var existing = await agents.FindByNameAsync(name, cancellationToken);
        if (existing is not null)
        {
            throw new EnrollmentException(
                EnrollmentFailure.AlreadyExists,
                $"Agent '{name}' already exists.");
        }

        var agent = new Agent
        {
            Name = name,
            Version = "0",
            AbiVersions = [],
            Labels = new Dictionary<string, string>(labels),
            Status = AgentStatus.Pending,
        };
        await agents.UpsertAsync(agent, cancellationToken);

        var token = await IssueTokenAsync(agent.Id, null, 1, TimeSpan.FromMinutes(15), cancellationToken);
        return new AgentProvisioning(agent.Id, token);
    }

    /// <inheritdoc />
    public Task<RegistrationToken> CreateRegistrationTokenAsync(
        string? namePrefix,
        int maxUses,
        TimeSpan ttl,
        string actor,
        CancellationToken cancellationToken)
    {
        if (maxUses <= 0)
        {
            throw new EnrollmentException(EnrollmentFailure.CsrInvalid, "maxUses must be positive.");
        }

        var token = GenerateToken();
        var record = new EnrollmentToken
        {
            TokenHash = HashToken(token),
            AgentId = null,
            NamePrefix = string.IsNullOrWhiteSpace(namePrefix) ? null : namePrefix,
            MaxUses = maxUses,
            ExpiresAt = DateTimeOffset.UtcNow + ttl,
        };
        tokens.AddAsync(record, cancellationToken);

        return Task.FromResult(new RegistrationToken(token, maxUses, record.ExpiresAt));
    }

    /// <inheritdoc />
    public async Task<EnrollmentResult> EnrollAsync(
        string agentId,
        string token,
        string csrPem,
        CancellationToken cancellationToken)
    {
        if (!Guid.TryParse(agentId, out var agentGuid))
        {
            throw new EnrollmentException(EnrollmentFailure.CsrInvalid, "Agent ID is not a valid GUID.");
        }

        var record = await tokens.FindByHashAsync(HashToken(token ?? string.Empty), cancellationToken);
        if (record is null)
        {
            throw new EnrollmentException(EnrollmentFailure.InvalidToken, "Enrollment token is invalid.");
        }

        if (record.ExpiresAt < DateTimeOffset.UtcNow)
        {
            throw new EnrollmentException(EnrollmentFailure.Expired, "Enrollment token has expired.");
        }

        if (record.UsedCount >= record.MaxUses)
        {
            throw new EnrollmentException(EnrollmentFailure.Exhausted, "Enrollment token is exhausted.");
        }

        if (record.AgentId is { } boundId && boundId != agentGuid)
        {
            throw new EnrollmentException(
                EnrollmentFailure.AgentMismatch,
                "Enrollment token is bound to a different agent.");
        }

        if (record.NamePrefix is { } prefix &&
            !agentGuid.ToString("D").StartsWith(prefix, StringComparison.OrdinalIgnoreCase))
        {
            throw new EnrollmentException(
                EnrollmentFailure.AgentMismatch,
                "Agent ID does not match the registration token scope.");
        }

        var agent = await agents.FindAsync(agentGuid, cancellationToken);
        if (agent is null)
        {
            if (record.AgentId is not null)
            {
                throw new EnrollmentException(
                    EnrollmentFailure.AgentMismatch,
                    "Agent does not exist.");
            }

            agent = new Agent
            {
                Id = agentGuid,
                Name = agentGuid.ToString("D"),
                Version = "0",
                AbiVersions = [],
                Labels = new Dictionary<string, string>(),
                Status = AgentStatus.Online,
            };
            await agents.UpsertAsync(agent, cancellationToken);
        }

        var commonName = AgentCertificatePrefix(agentGuid);
        var notAfter = DateTimeOffset.UtcNow + options.AgentCertificateLifetime;
        var issued = await authority.IssueAsync(csrPem, commonName, notAfter, cancellationToken);

        await certificates.AddAsync(
            new AgentCertificate
            {
                AgentId = agentGuid,
                SerialNumber = issued.SerialNumber,
                Thumbprint = issued.Thumbprint,
                Pem = issued.CertificatePem,
                NotBefore = issued.NotBefore,
                NotAfter = issued.NotAfter,
            },
            cancellationToken);

        await tokens.ConsumeAsync(record.Id, cancellationToken);

        var caPem = await authority.GetCaCertificatePemAsync(cancellationToken);
        return new EnrollmentResult(issued.CertificatePem, caPem, issued.NotAfter);
    }

    /// <summary>Creates and stores a one-time token bound to one agent.</summary>
    /// <param name="agentId">Agent the token is bound to.</param>
    /// <param name="namePrefix">Unused for per-agent tokens.</param>
    /// <param name="maxUses">Maximum number of uses.</param>
    /// <param name="ttl">Token lifetime.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The raw token value.</returns>
    private async Task<string> IssueTokenAsync(
        Guid agentId,
        string? namePrefix,
        int maxUses,
        TimeSpan ttl,
        CancellationToken cancellationToken)
    {
        var raw = GenerateToken();
        await tokens.AddAsync(
            new EnrollmentToken
            {
                TokenHash = HashToken(raw),
                AgentId = agentId,
                NamePrefix = namePrefix,
                MaxUses = maxUses,
                ExpiresAt = DateTimeOffset.UtcNow + ttl,
            },
            cancellationToken);
        return raw;
    }

    /// <summary>Builds the certificate subject CN for an agent.</summary>
    /// <param name="agentId">Agent identifier.</param>
    private static string AgentCertificatePrefix(Guid agentId) => $"wasmgate-agent:{agentId:D}";

    /// <summary>Generates a URL-safe random token value.</summary>
    private static string GenerateToken() =>
        Convert.ToBase64String(RandomNumberGenerator.GetBytes(32))
            .TrimEnd('=')
            .Replace('+', '-')
            .Replace('/', '_');

    /// <summary>Hashes a raw token for at-rest storage.</summary>
    /// <param name="raw">Raw token value.</param>
    private static string HashToken(string raw) =>
        Convert.ToHexString(SHA256.HashData(Encoding.UTF8.GetBytes(raw)));
}
