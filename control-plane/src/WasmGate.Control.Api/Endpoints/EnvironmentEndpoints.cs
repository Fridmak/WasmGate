using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities;

namespace WasmGate.Control.Api.Endpoints;

/// <summary>Environment catalog endpoints used by the admin picker.</summary>
internal static class EnvironmentEndpoints
{
    /// <summary>Maps the environment routes.</summary>
    /// <param name="app">Endpoint route builder to extend.</param>
    /// <returns>The same route builder for chaining.</returns>
    public static IEndpointRouteBuilder MapEnvironmentEndpoints(this IEndpointRouteBuilder app)
    {
        app.MapGet("/api/v1/environments", ListEnvironments);
        app.MapPost("/api/v1/environments", CreateEnvironment);
        app.MapDelete("/api/v1/environments/{name}", DeleteEnvironment);
        return app;
    }

    /// <summary>Lists every environment known to the control plane.</summary>
    /// <param name="repository">Environment catalog.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> ListEnvironments(
        IEnvironmentRepository repository,
        CancellationToken cancellationToken)
    {
        var names = await repository.ListAsync(cancellationToken);
        return Results.Ok(names.Select(name => new EnvironmentSummary(name)).ToList());
    }

    /// <summary>Creates an environment; creating an existing one is idempotent.</summary>
    /// <param name="request">Environment name.</param>
    /// <param name="repository">Environment catalog.</param>
    /// <param name="audit">Audit log.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>The normalised environment name; 400 when the name is invalid.</returns>
    private static async Task<IResult> CreateEnvironment(
        CreateEnvironmentRequest request,
        IEnvironmentRepository repository,
        IAuditLog audit,
        CancellationToken cancellationToken)
    {
        var name = EnvironmentNaming.Normalize(request.Name);
        if (!EnvironmentNaming.IsValid(name))
        {
            return Results.BadRequest(
                $"Environment must be 1-{EnvironmentNaming.MaxLength} ASCII letters, digits, " +
                "'.', '_' or '-', starting and ending with a letter or digit.");
        }

        if (await repository.EnsureAsync(name, "admin-api", cancellationToken))
        {
            await audit.AppendAsync(
                new AuditEvent
                {
                    Actor = "admin-api",
                    Action = "environment.create",
                    Target = name,
                },
                cancellationToken);
        }

        return Results.Ok(new EnvironmentSummary(name));
    }

    /// <summary>Deletes an environment and all data scoped to it.</summary>
    /// <param name="name">Environment name.</param>
    /// <param name="repository">Environment catalog.</param>
    /// <param name="audit">Audit log.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>
    /// 204 on success, 404 for an unknown environment, 409 when it is the default
    /// environment, still has agents, or has a running deployment.
    /// </returns>
    private static async Task<IResult> DeleteEnvironment(
        string name,
        IEnvironmentRepository repository,
        IAuditLog audit,
        CancellationToken cancellationToken)
    {
        var normalized = EnvironmentNaming.Normalize(name);
        if (!EnvironmentNaming.IsValid(normalized))
        {
            return Results.BadRequest("Environment name is not valid.");
        }

        try
        {
            await repository.DeleteAsync(normalized, cancellationToken);
            await audit.AppendAsync(
                new AuditEvent
                {
                    Actor = "admin-api",
                    Action = "environment.delete",
                    Target = normalized,
                },
                cancellationToken);
            return Results.NoContent();
        }
        catch (KeyNotFoundException)
        {
            return Results.NotFound();
        }
        catch (InvalidOperationException ex)
        {
            return Results.Conflict(ex.Message);
        }
    }
}
