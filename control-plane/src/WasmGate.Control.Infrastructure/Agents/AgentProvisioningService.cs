using System.Security.Cryptography;
using System.Text;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Core.Entities.Agents.Enrollment;

namespace WasmGate.Control.Infrastructure.Agents;

/// <summary>
/// Provisions agents and enrolls data planes: creates agents with a single-use
/// enrollment token, issues shared registration tokens, validates tokens and
/// CSRs, and records the certificates it issues.
/// </summary>
/// <remarks>
/// A use of a registration token is reserved with one conditional update, so
/// several control-plane instances can serve enrollments concurrently without
/// exceeding the use limit. Raw tokens are returned once and stored only as
/// SHA-256 hashes.
/// </remarks>
public sealed class AgentProvisioningService(
    IAgentRepository agents,
    IEnrollmentTokenRepository tokens,
    IAgentCertificateRepository certificates,
    IEnvironmentRepository environments,
    IEnvironmentOperationLock environmentLocks,
    ICertificateAuthority authority,
    IAgentSessionRegistry sessions,
    IAuditLog audit,
    ControlPlaneOptions options) : IAgentProvisioningService
{
    /// <summary>Maximum length of a label key accepted from the admin API.</summary>
    private const int MaxLabelKeyLength = 63;

    /// <summary>Maximum length of a label value accepted from the admin API.</summary>
    private const int MaxLabelValueLength = 256;

    /// <inheritdoc />
    public async Task<AgentProvisioning> CreateAgentAsync(
        string name,
        string? environment,
        IReadOnlyDictionary<string, string> labels,
        string actor,
        CancellationToken cancellationToken)
    {
        ArgumentNullException.ThrowIfNull(labels);

        var normalizedName = name?.Trim() ?? string.Empty;
        if (!AgentNaming.IsValid(normalizedName))
        {
            throw new EnrollmentException(
                EnrollmentFailure.InvalidRequest,
                $"Agent name must match {AgentNamingValidNameHint}.");
        }

        var normalizedEnvironment = NormalizeEnvironment(environment);
        await environments.EnsureAsync(normalizedEnvironment, actor, cancellationToken);

        var existing = await agents.FindByNameAsync(normalizedName, cancellationToken);
        if (existing is not null)
        {
            throw new EnrollmentException(
                EnrollmentFailure.AlreadyExists,
                $"Agent '{normalizedName}' already exists.");
        }

        var agent = new Agent
        {
            Name = normalizedName,
            Environment = normalizedEnvironment,
            Version = "0",
            AbiVersions = [],
            Labels = CopyLabels(labels),
            Status = AgentStatus.Pending,
        };
        if (!await agents.UpsertAsync(agent, cancellationToken))
        {
            throw new EnrollmentException(
                EnrollmentFailure.InvalidRequest,
                "Agent was deleted and cannot be recreated with the same id.");
        }
        await AuditAsync(actor, "agent.create", agent.Id.ToString("D"), cancellationToken);

        var token = await IssueTokenAsync(
            agent.Id,
            normalizedEnvironment,
            null,
            1,
            DateTimeOffset.UtcNow + TimeSpan.FromMinutes(15),
            cancellationToken);
        await AuditAsync(actor, "token.create", agent.Id.ToString("D"), cancellationToken);

        return new AgentProvisioning(agent.Id, token);
    }

    /// <inheritdoc />
    public async Task<RegistrationToken> CreateRegistrationTokenAsync(
        string? namePrefix,
        string? environment,
        int maxUses,
        TimeSpan ttl,
        string actor,
        CancellationToken cancellationToken)
    {
        if (maxUses <= 0)
        {
            throw new EnrollmentException(
                EnrollmentFailure.InvalidRequest,
                "maxUses must be greater than zero.");
        }

        if (ttl <= TimeSpan.Zero)
        {
            throw new EnrollmentException(
                EnrollmentFailure.InvalidRequest,
                "Token lifetime must be greater than zero.");
        }

        var normalizedPrefix = string.IsNullOrWhiteSpace(namePrefix) ? null : namePrefix.Trim();
        if (normalizedPrefix is not null &&
            (normalizedPrefix.Length > AgentNaming.MaxLength ||
             normalizedPrefix.Any(c => !char.IsAsciiLetterOrDigit(c) && c is not ('.' or '_' or '-'))))
        {
            throw new EnrollmentException(
                EnrollmentFailure.InvalidRequest,
                "namePrefix may contain only ASCII letters, digits, '.', '_' and '-'.");
        }

        var normalizedEnvironment = NormalizeEnvironment(environment);
        await using var environmentLock = await environmentLocks.AcquireAsync(
            normalizedEnvironment,
            cancellationToken);
        await environments.EnsureAsync(normalizedEnvironment, actor, cancellationToken);

        var token = GenerateToken();
        var record = new EnrollmentToken
        {
            TokenHash = HashToken(token),
            AgentId = null,
            Environment = normalizedEnvironment,
            NamePrefix = normalizedPrefix,
            MaxUses = maxUses,
            ExpiresAt = DateTimeOffset.UtcNow + ttl,
        };
        await tokens.AddAsync(record, cancellationToken);
        await AuditAsync(actor, "token.create", record.Id.ToString("D"), cancellationToken);

        return new RegistrationToken(token, maxUses, record.ExpiresAt);
    }

    /// <inheritdoc />
    public async Task<EnrollmentResult> EnrollAsync(
        string agentId,
        string? name,
        string token,
        string csrPem,
        CancellationToken cancellationToken)
    {
        if (!Guid.TryParse(agentId?.Trim(), out var agentGuid))
        {
            throw new EnrollmentException(
                EnrollmentFailure.InvalidRequest,
                "Agent ID is not a valid GUID.");
        }

        var rawToken = token?.Trim() ?? string.Empty;
        if (rawToken.Length == 0)
        {
            throw new EnrollmentException(EnrollmentFailure.InvalidToken, "Enrollment token is invalid.");
        }

        var tokenHash = HashToken(rawToken);
        var record = await tokens.FindByHashAsync(tokenHash, cancellationToken);
        if (record is null)
        {
            throw new EnrollmentException(EnrollmentFailure.InvalidToken, "Enrollment token is invalid.");
        }

        // The first read only discovers the lock scope. The token is read again
        // after the lock is held, so an environment deletion that removed the
        // token cannot be overtaken by this enrollment.
        var lockEnvironment = EnvironmentNaming.Normalize(record.Environment);
        if (lockEnvironment.Length > 0 && !EnvironmentNaming.IsValid(lockEnvironment))
        {
            throw new EnrollmentException(
                EnrollmentFailure.InvalidRequest,
                "Enrollment token carries an invalid environment.");
        }

        if (lockEnvironment.Length == 0 && record.AgentId is { } boundForLock)
        {
            var boundAgent = await agents.FindAsync(boundForLock, cancellationToken);
            if (boundAgent is not null)
            {
                lockEnvironment = boundAgent.Environment;
            }
        }

        if (lockEnvironment.Length == 0)
        {
            lockEnvironment = EnvironmentNaming.Default;
        }

        await using var environmentLock = await environmentLocks.AcquireAsync(
            lockEnvironment,
            cancellationToken);

        record = await tokens.FindByHashAsync(tokenHash, cancellationToken);
        if (record is null)
        {
            throw new EnrollmentException(EnrollmentFailure.InvalidToken, "Enrollment token is invalid.");
        }

        var now = DateTimeOffset.UtcNow;
        if (record.ExpiresAt <= now)
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

        var tokenEnvironment = EnvironmentNaming.Normalize(record.Environment);
        if (tokenEnvironment.Length > 0 && !EnvironmentNaming.IsValid(tokenEnvironment))
        {
            throw new EnrollmentException(
                EnrollmentFailure.InvalidRequest,
                "Enrollment token carries an invalid environment.");
        }

        var existing = await agents.FindAsync(agentGuid, cancellationToken);
        var requestedName = name?.Trim();
        var isNewAgent = existing is null;

        if (existing is not null &&
            tokenEnvironment.Length > 0 &&
            !string.Equals(existing.Environment, tokenEnvironment, StringComparison.OrdinalIgnoreCase))
        {
            throw new EnrollmentException(
                EnrollmentFailure.AgentMismatch,
                $"Enrollment token belongs to environment '{tokenEnvironment}', " +
                $"but agent '{existing.Name}' is in '{existing.Environment}'.");
        }

        if (existing is null)
        {
            var effectiveName = string.IsNullOrEmpty(requestedName)
                ? agentGuid.ToString("D")
                : requestedName;

            if (!AgentNaming.IsValid(effectiveName))
            {
                throw new EnrollmentException(
                    EnrollmentFailure.InvalidRequest,
                    $"Agent name must match {AgentNamingValidNameHint}.");
            }

            if (await agents.FindByNameAsync(effectiveName, cancellationToken) is not null)
            {
                throw new EnrollmentException(
                    EnrollmentFailure.AlreadyExists,
                    $"Agent '{effectiveName}' already exists.");
            }

            var environment = tokenEnvironment.Length > 0
                ? tokenEnvironment
                : EnvironmentNaming.Default;

            existing = new Agent
            {
                Id = agentGuid,
                Name = effectiveName,
                Environment = environment,
                Version = "0",
                AbiVersions = [],
                Labels = new Dictionary<string, string>(StringComparer.Ordinal),
                Status = AgentStatus.Pending,
            };
        }
        else if (!string.IsNullOrEmpty(requestedName) &&
                 !string.Equals(requestedName, existing.Name, StringComparison.Ordinal))
        {
            throw new EnrollmentException(
                EnrollmentFailure.AgentMismatch,
                "Agent name does not match the registered name.");
        }

        if (record.NamePrefix is { Length: > 0 } prefix &&
            !existing.Name.StartsWith(prefix, StringComparison.Ordinal))
        {
            throw new EnrollmentException(
                EnrollmentFailure.AgentMismatch,
                "Agent name does not match the registration token scope.");
        }

        // Reserve the use before issuing anything. A malformed CSR burns one
        // use, but a read-then-write check would let concurrent enrollments
        // exceed max_uses.
        if (!await tokens.ConsumeAsync(record.Id, cancellationToken))
        {
            throw new EnrollmentException(EnrollmentFailure.Exhausted, "Enrollment token is exhausted.");
        }

        if (isNewAgent)
        {
            if (!await agents.UpsertAsync(existing, cancellationToken))
            {
                throw new EnrollmentException(
                    EnrollmentFailure.InvalidRequest,
                    "Agent was deleted and cannot be re-enrolled; create a new agent record.");
            }

            await AuditAsync(
                AgentNaming.CertificateCommonName(agentGuid),
                "agent.create",
                agentGuid.ToString("D"),
                cancellationToken);
        }
        else
        {
            // Enrollment proves the identity, not that a control session is open:
            // record only the contact and leave status and release to the session
            // handler. A single-column update cannot overwrite concurrent state.
            await agents.RecordEnrollmentAsync(agentGuid, now, cancellationToken);
        }

        var notAfter = now + options.AgentCertificateLifetime;
        var issued = await authority.IssueAsync(
            csrPem,
            AgentNaming.CertificateCommonName(agentGuid),
            notAfter,
            cancellationToken);

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

        await AuditAsync(
            AgentNaming.CertificateCommonName(agentGuid),
            "agent.enroll",
            agentGuid.ToString("D"),
            cancellationToken);

        var caPem = await authority.GetCaCertificatePemAsync(cancellationToken);
        return new EnrollmentResult(issued.CertificatePem, caPem, issued.NotAfter);
    }

    /// <inheritdoc />
    public async Task<RegistrationToken> IssueEnrollmentTokenAsync(
        Guid agentId,
        string actor,
        CancellationToken cancellationToken)
    {
        var agent = await agents.FindAsync(agentId, cancellationToken);
        if (agent is null)
        {
            throw new EnrollmentException(
                EnrollmentFailure.InvalidRequest,
                "Agent is not registered.");
        }

        var ttl = TimeSpan.FromMinutes(15);
        var expiresAt = DateTimeOffset.UtcNow + ttl;
        var token = await IssueTokenAsync(
            agentId,
            agent.Environment,
            null,
            1,
            expiresAt,
            cancellationToken);
        await AuditAsync(actor, "token.create", agentId.ToString("D"), cancellationToken);

        return new RegistrationToken(token, 1, expiresAt);
    }

    /// <inheritdoc />
    public async Task SetEnvironmentAsync(
        Guid agentId,
        string environment,
        string actor,
        CancellationToken cancellationToken)
    {
        if (string.IsNullOrWhiteSpace(environment))
        {
            throw new EnrollmentException(
                EnrollmentFailure.InvalidRequest,
                "Environment is required.");
        }

        var normalizedEnvironment = NormalizeEnvironment(environment);
        await using var environmentLock = await environmentLocks.AcquireAsync(
            normalizedEnvironment,
            cancellationToken);
        var agent = await agents.FindAsync(agentId, cancellationToken);
        if (agent is null)
        {
            throw new EnrollmentException(
                EnrollmentFailure.InvalidRequest,
                "Agent is not registered.");
        }

        if (string.Equals(agent.Environment, normalizedEnvironment, StringComparison.OrdinalIgnoreCase))
        {
            return;
        }

        await environments.EnsureAsync(normalizedEnvironment, actor, cancellationToken);
        await agents.SetEnvironmentAsync(agentId, normalizedEnvironment, cancellationToken);
        await AuditAsync(actor, "agent.move-environment", agentId.ToString("D"), cancellationToken);
    }

    /// <inheritdoc />
    public async Task DeleteAgentAsync(
        Guid agentId,
        string actor,
        CancellationToken cancellationToken)
    {
        if (!await agents.DeleteAsync(agentId, cancellationToken))
        {
            throw new KeyNotFoundException($"Agent '{agentId:D}' is not registered.");
        }

        // Close the session after the row is gone: the session loop's cleanup
        // must not be able to re-insert an Offline row for a deleted agent.
        if (sessions.TryGet(agentId, out var session))
        {
            await session.CloseAsync(CancellationToken.None);
        }

        await AuditAsync(actor, "agent.delete", agentId.ToString("D"), cancellationToken);
    }

    /// <inheritdoc />
    public async Task<EnrollmentResult> RenewAsync(
        Guid agentId,
        string csrPem,
        CancellationToken cancellationToken)
    {
        if (await agents.FindAsync(agentId, cancellationToken) is null)
        {
            throw new EnrollmentException(
                EnrollmentFailure.InvalidRequest,
                "Agent is not registered.");
        }

        var notAfter = DateTimeOffset.UtcNow + options.AgentCertificateLifetime;
        var issued = await authority.IssueAsync(
            csrPem,
            AgentNaming.CertificateCommonName(agentId),
            notAfter,
            cancellationToken);

        await certificates.AddAsync(
            new AgentCertificate
            {
                AgentId = agentId,
                SerialNumber = issued.SerialNumber,
                Thumbprint = issued.Thumbprint,
                Pem = issued.CertificatePem,
                NotBefore = issued.NotBefore,
                NotAfter = issued.NotAfter,
            },
            cancellationToken);

        await agents.RecordEnrollmentAsync(agentId, DateTimeOffset.UtcNow, cancellationToken);
        await AuditAsync(
            AgentNaming.CertificateCommonName(agentId),
            "agent.renew",
            agentId.ToString("D"),
            cancellationToken);

        var caPem = await authority.GetCaCertificatePemAsync(cancellationToken);
        return new EnrollmentResult(issued.CertificatePem, caPem, issued.NotAfter);
    }

    /// <summary>Human-readable description of the agent-name rules used in errors.</summary>
    private static string AgentNamingValidNameHint =>
        $"1-{AgentNaming.MaxLength} ASCII letters, digits, '.', '_' or '-', " +
        "starting and ending with a letter or digit";

    /// <summary>Normalises an environment name and defaults it to <c>prod</c>.</summary>
    /// <param name="environment">Operator-supplied name, or null.</param>
    /// <returns>A valid lower-case environment name.</returns>
    /// <exception cref="EnrollmentException">The name is not acceptable.</exception>
    private static string NormalizeEnvironment(string? environment)
    {
        var normalized = EnvironmentNaming.Normalize(environment);
        if (normalized.Length == 0)
        {
            return EnvironmentNaming.Default;
        }

        if (!EnvironmentNaming.IsValid(normalized))
        {
            throw new EnrollmentException(
                EnrollmentFailure.InvalidRequest,
                $"Environment must be 1-{EnvironmentNaming.MaxLength} ASCII letters, digits, " +
                "'.', '_' or '-', starting and ending with a letter or digit.");
        }

        return normalized;
    }

    /// <summary>Creates and stores a single-use token bound to one agent.</summary>
    /// <param name="agentId">Agent the token is bound to.</param>
    /// <param name="environment">Environment a newly enrolled agent joins.</param>
    /// <param name="namePrefix">Optional name prefix, normally null for per-agent tokens.</param>
    /// <param name="maxUses">Maximum number of uses.</param>
    /// <param name="expiresAt">Absolute expiry time of the token.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The raw token value.</returns>
    private async Task<string> IssueTokenAsync(
        Guid agentId,
        string environment,
        string? namePrefix,
        int maxUses,
        DateTimeOffset expiresAt,
        CancellationToken cancellationToken)
    {
        var raw = GenerateToken();
        await tokens.AddAsync(
            new EnrollmentToken
            {
                TokenHash = HashToken(raw),
                AgentId = agentId,
                Environment = environment,
                NamePrefix = namePrefix,
                MaxUses = maxUses,
                ExpiresAt = expiresAt,
            },
            cancellationToken);
        return raw;
    }

    /// <summary>Copies and validates operator-supplied labels.</summary>
    /// <param name="labels">Labels from the admin API.</param>
    /// <returns>A mutable copy that can be stored in the agent entity.</returns>
    /// <exception cref="EnrollmentException">A label key or value is not acceptable.</exception>
    private static Dictionary<string, string> CopyLabels(IReadOnlyDictionary<string, string> labels)
    {
        var copy = new Dictionary<string, string>(labels.Count, StringComparer.Ordinal);
        foreach (var pair in labels)
        {
            if (string.IsNullOrWhiteSpace(pair.Key) || pair.Key.Length > MaxLabelKeyLength)
            {
                throw new EnrollmentException(
                    EnrollmentFailure.InvalidRequest,
                    $"Label keys must be non-empty and at most {MaxLabelKeyLength} characters.");
            }

            if (pair.Value is null || pair.Value.Length > MaxLabelValueLength)
            {
                throw new EnrollmentException(
                    EnrollmentFailure.InvalidRequest,
                    $"Label values must be non-null and at most {MaxLabelValueLength} characters.");
            }

            copy[pair.Key] = pair.Value;
        }

        return copy;
    }

    /// <summary>Appends one audit event for a provisioning operation.</summary>
    /// <param name="actor">Principal that performed the operation.</param>
    /// <param name="action">Stable action name.</param>
    /// <param name="target">Entity the action applies to.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private Task AuditAsync(string actor, string action, string target, CancellationToken cancellationToken) =>
        audit.AppendAsync(
            new AuditEvent
            {
                Actor = actor,
                Action = action,
                Target = target,
            },
            cancellationToken);

    /// <summary>Generates a URL-safe random token value.</summary>
    /// <returns>43 characters of URL-safe base64 without padding.</returns>
    private static string GenerateToken() =>
        Convert.ToBase64String(RandomNumberGenerator.GetBytes(32))
            .TrimEnd('=')
            .Replace('+', '-')
            .Replace('/', '_');

    /// <summary>Hashes a raw token for at-rest storage.</summary>
    /// <param name="raw">Raw token value.</param>
    /// <returns>Lower-case hex SHA-256.</returns>
    private static string HashToken(string raw) =>
        Convert.ToHexString(SHA256.HashData(Encoding.UTF8.GetBytes(raw))).ToLowerInvariant();
}
