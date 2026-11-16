using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Agents.Enrollment;

namespace WasmGate.Control.Api.Endpoints;

/// <summary>Agent provisioning and registration-token endpoints.</summary>
internal static class AgentEndpoints
{
    /// <summary>Maps the agent and registration-token routes.</summary>
    /// <param name="app">Endpoint route builder to extend.</param>
    /// <returns>The same route builder for chaining.</returns>
    public static IEndpointRouteBuilder MapAgentEndpoints(this IEndpointRouteBuilder app)
    {
        app.MapPost("/api/v1/agents", CreateAgent);
        app.MapGet("/api/v1/agents", ListAgents);
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
            .Select(a => new AgentSummary(a.Id, a.Name, a.Status, a.LastSeenAt, a.ActiveReleaseId))
            .ToList());
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
