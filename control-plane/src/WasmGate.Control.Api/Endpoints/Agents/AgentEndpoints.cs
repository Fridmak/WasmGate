using WasmGate.Control.Api.Contracts;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.Core.Entities.Agents.Enrollment;

namespace WasmGate.Control.Api.Endpoints.Agents;

/// <summary>Agent provisioning, enrollment-token and certificate endpoints.</summary>
internal static class AgentEndpoints
{
    /// <summary>Maps the agent, token and certificate routes.</summary>
    /// <param name="app">Endpoint route builder to extend.</param>
    /// <returns>The same route builder for chaining.</returns>
    public static IEndpointRouteBuilder MapAgentEndpoints(this IEndpointRouteBuilder app)
    {
        app.MapPost("/api/v1/agents", CreateAgent);
        app.MapGet("/api/v1/agents", ListAgents);
        app.MapPost("/api/v1/agents/{id:guid}/tokens", IssueEnrollmentToken);
        app.MapPost("/api/v1/agents/{id:guid}/environment", SetAgentEnvironment);
        app.MapDelete("/api/v1/agents/{id:guid}", DeleteAgent);
        app.MapGet("/api/v1/agents/{id:guid}/certificates", ListCertificates);
        app.MapPost("/api/v1/agents/{id:guid}/certificates/{serial}/revoke", RevokeCertificate);
        app.MapPost("/api/v1/registration-tokens", CreateRegistrationToken);
        return app;
    }

    /// <summary>Creates one agent and returns a one-time enrollment token.</summary>
    /// <param name="request">Agent name and labels.</param>
    /// <param name="provisioning">Provisioning service.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>
    /// The new agent identifier and its raw token, which is shown only here.
    /// Returns 409 when the name is already registered.
    /// </returns>
    private static async Task<IResult> CreateAgent(
        CreateAgentRequest request,
        IAgentProvisioningService provisioning,
        CancellationToken cancellationToken)
    {
        try
        {
            var result = await provisioning.CreateAgentAsync(
                request.Name,
                request.Environment,
                request.Labels ?? new Dictionary<string, string>(),
                actor: "admin-api",
                cancellationToken);

            return Results.Ok(new CreateAgentResponse(result.AgentId, result.Token));
        }
        catch (EnrollmentException ex) when (ex.Failure == EnrollmentFailure.AlreadyExists)
        {
            return Results.Conflict(ex.Message);
        }
        catch (EnrollmentException ex)
        {
            return Results.BadRequest(ex.Message);
        }
    }

    /// <summary>Lists registered agents.</summary>
    /// <param name="agents">Agent repository.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> ListAgents(
        IAgentRepository agents,
        CancellationToken cancellationToken)
    {
        var list = await agents.ListAsync(cancellationToken);
        return Results.Ok(list
            .Select(a => new AgentSummary(
                a.Id,
                a.Name,
                a.Environment,
                a.Status,
                a.LastSeenAt,
                a.ActiveReleaseId,
                a.Labels))
            .ToList());
    }

    /// <summary>Moves an agent to another environment.</summary>
    /// <param name="id">Registered agent identifier.</param>
    /// <param name="request">Target environment.</param>
    /// <param name="provisioning">Provisioning service.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>204 on success; 400 for an unknown agent or an invalid name.</returns>
    private static async Task<IResult> SetAgentEnvironment(
        Guid id,
        SetAgentEnvironmentRequest request,
        IAgentProvisioningService provisioning,
        CancellationToken cancellationToken)
    {
        try
        {
            await provisioning.SetEnvironmentAsync(
                id,
                request.Environment,
                actor: "admin-api",
                cancellationToken);
            return Results.NoContent();
        }
        catch (EnrollmentException ex)
        {
            return Results.BadRequest(ex.Message);
        }
    }

    /// <summary>Issues a replacement single-use enrollment token for an existing agent.</summary>
    /// <param name="id">Registered agent identifier.</param>
    /// <param name="provisioning">Provisioning service.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The raw token and its expiry; 400 for an unknown agent.</returns>
    private static async Task<IResult> IssueEnrollmentToken(
        Guid id,
        IAgentProvisioningService provisioning,
        CancellationToken cancellationToken)
    {
        try
        {
            var token = await provisioning.IssueEnrollmentTokenAsync(
                id,
                actor: "admin-api",
                cancellationToken);
            return Results.Ok(new CreateAgentTokenResponse(token.Token, token.ExpiresAt));
        }
        catch (EnrollmentException ex)
        {
            return Results.BadRequest(ex.Message);
        }
    }

    /// <summary>Deletes an agent, its certificates and its enrollment tokens.</summary>
    /// <param name="id">Registered agent identifier.</param>
    /// <param name="provisioning">Provisioning service.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>
    /// 204 on success, 404 for an unknown agent. A live session is closed; the
    /// data-plane process must be stopped separately.
    /// </returns>
    private static async Task<IResult> DeleteAgent(
        Guid id,
        IAgentProvisioningService provisioning,
        CancellationToken cancellationToken)
    {
        try
        {
            await provisioning.DeleteAgentAsync(
                id,
                actor: "admin-api",
                cancellationToken);
            return Results.NoContent();
        }
        catch (KeyNotFoundException)
        {
            return Results.NotFound();
        }
        catch (EnrollmentException ex)
        {
            return Results.BadRequest(ex.Message);
        }
    }

    /// <summary>Lists the active certificates of an agent.</summary>
    /// <param name="id">Agent identifier.</param>
    /// <param name="certificates">Certificate repository.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>Serial, fingerprint and validity period of each active certificate.</returns>
    private static async Task<IResult> ListCertificates(
        Guid id,
        IAgentCertificateRepository certificates,
        CancellationToken cancellationToken)
    {
        var active = await certificates.ListActiveByAgentAsync(id, cancellationToken);
        return Results.Ok(active
            .Select(certificate => new AgentCertificateSummary(
                certificate.SerialNumber,
                certificate.Thumbprint,
                certificate.NotBefore,
                certificate.NotAfter,
                certificate.IssuedAt))
            .ToList());
    }

    /// <summary>Marks one issued certificate as revoked.</summary>
    /// <param name="id">Agent identifier.</param>
    /// <param name="serial">Certificate serial number.</param>
    /// <param name="certificates">Certificate repository.</param>
    /// <param name="sessions">Live agent session registry.</param>
    /// <param name="audit">Audit log.</param>
    /// <param name="logger">Logger for post-commit cleanup failures.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>204 on success; 404 when the certificate is unknown or already revoked.</returns>
    private static async Task<IResult> RevokeCertificate(
        Guid id,
        string serial,
        IAgentCertificateRepository certificates,
        IAgentSessionRegistry sessions,
        IAuditLog audit,
        ILogger<Program> logger,
        CancellationToken cancellationToken)
    {
        // The database update must happen before the registry lookup. Together
        // with the post-register check in AgentControlService, this guarantees
        // that a concurrent session either sees the revoked certificate or is
        // found in the registry and closed.
        if (!await certificates.RevokeAsync(id, serial, cancellationToken))
        {
            return Results.NotFound();
        }

        if (sessions.TryGet(id, out var session))
        {
            try
            {
                sessions.Retire(session);
                await session.CloseAsync(CancellationToken.None);
            }
            catch (Exception ex)
            {
                logger.LogWarning(
                    ex,
                    "failed to close session after certificate revocation: agent={AgentId} serial={Serial}",
                    id,
                    serial);
            }
        }

        try
        {
            await audit.AppendAsync(
                new AuditEvent
                {
                    Actor = "admin-api",
                    Action = "certificate.revoke",
                    Target = $"{id:D}/{serial}",
                },
                CancellationToken.None);
        }
        catch (Exception ex)
        {
            logger.LogWarning(
                ex,
                "failed to append audit event after certificate revocation: agent={AgentId} serial={Serial}",
                id,
                serial);
        }

        return Results.NoContent();
    }

    /// <summary>Creates a registration token that many agents can share.</summary>
    /// <param name="request">Token scope and limits.</param>
    /// <param name="provisioning">Provisioning service.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The raw token, its use limit and expiry.</returns>
    private static async Task<IResult> CreateRegistrationToken(
        CreateRegistrationTokenRequest request,
        IAgentProvisioningService provisioning,
        CancellationToken cancellationToken)
    {
        try
        {
            var result = await provisioning.CreateRegistrationTokenAsync(
                request.NamePrefix,
                request.Environment,
                request.MaxUses,
                TimeSpan.FromSeconds(request.TtlSeconds),
                actor: "admin-api",
                cancellationToken);

            return Results.Ok(new CreateRegistrationTokenResponse(result.Token, result.MaxUses, result.ExpiresAt));
        }
        catch (EnrollmentException ex)
        {
            return Results.BadRequest(ex.Message);
        }
    }
}
