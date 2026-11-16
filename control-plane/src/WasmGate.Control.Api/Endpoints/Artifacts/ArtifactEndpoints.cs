using WasmGate.Control.Api.Contracts;
using WasmGate.Control.Core.Abstractions.Releases;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Configuration;

namespace WasmGate.Control.Api.Endpoints.Artifacts;

/// <summary>Artifact upload and listing endpoints.</summary>
internal static class ArtifactEndpoints
{
    /// <summary>Maps the artifact routes.</summary>
    /// <param name="app">Endpoint route builder to extend.</param>
    /// <returns>The same route builder for chaining.</returns>
    public static IEndpointRouteBuilder MapArtifactEndpoints(this IEndpointRouteBuilder app)
    {
        app.MapPost("/api/v1/artifacts", UploadArtifact);
        app.MapGet("/api/v1/artifacts", ListArtifacts);
        return app;
    }

    /// <summary>Lists stored artifacts, newest first.</summary>
    /// <param name="repository">Artifact metadata repository.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    private static async Task<IResult> ListArtifacts(
        IArtifactRepository repository,
        CancellationToken cancellationToken) =>
        Results.Ok(await repository.ListAsync(cancellationToken));

    /// <summary>Stores an artifact and its metadata.</summary>
    /// <param name="request">Artifact bytes, base64-encoded, and their metadata.</param>
    /// <param name="store">Content-addressed artifact store.</param>
    /// <param name="repository">Artifact metadata repository.</param>
    /// <param name="options">Size limit for one artifact.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>
    /// The content hash and stored size; re-uploading the same bytes returns the
    /// same hash and keeps the existing metadata. Returns 400 for an unknown kind,
    /// 413 when the artifact exceeds the limit.
    /// </returns>
    private static async Task<IResult> UploadArtifact(
        UploadArtifactRequest request,
        IArtifactStore store,
        IArtifactRepository repository,
        ControlPlaneOptions options,
        CancellationToken cancellationToken)
    {
        if (!Enum.IsDefined(request.Kind))
        {
            return Results.BadRequest($"Unknown artifact kind '{(int)request.Kind}'.");
        }

        byte[] content;
        try
        {
            content = Convert.FromBase64String(request.ContentBase64);
        }
        catch (FormatException)
        {
            return Results.BadRequest("ContentBase64 is not valid base64.");
        }

        if (content.LongLength > options.MaxArtifactBytes)
        {
            return Results.Text(
                $"Artifact exceeds the {options.MaxArtifactBytes} byte limit.",
                statusCode: StatusCodes.Status413PayloadTooLarge);
        }

        await using var stream = new MemoryStream(content, writable: false);
        var artifact = await store.PutAsync(
            stream,
            request.Kind,
            request.MediaType,
            createdBy: "admin-api",
            cancellationToken);
        await repository.AddAsync(artifact, cancellationToken);
        return Results.Ok(new UploadArtifactResponse(artifact.Id, artifact.Size));
    }
}
