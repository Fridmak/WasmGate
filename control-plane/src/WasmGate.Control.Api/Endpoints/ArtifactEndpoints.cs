using WasmGate.Control.Core.Abstractions.Releases;
using WasmGate.Control.Core.Abstractions.Repositories;

namespace WasmGate.Control.Api.Endpoints;

/// <summary>Artifact upload endpoint.</summary>
internal static class ArtifactEndpoints
{
    /// <summary>Maps <c>POST /api/v1/artifacts</c>.</summary>
    /// <param name="app">Endpoint route builder to extend.</param>
    /// <returns>The same route builder for chaining.</returns>
    public static IEndpointRouteBuilder MapArtifactEndpoints(this IEndpointRouteBuilder app)
    {
        app.MapPost("/api/v1/artifacts", UploadArtifact);
        return app;
    }

    /// <summary>Stores an artifact and its metadata.</summary>
    /// <param name="request">Artifact bytes, base64-encoded, and their metadata.</param>
    /// <param name="store">Content-addressed artifact store.</param>
    /// <param name="repository">Artifact metadata repository.</param>
    /// <param name="cancellationToken">Cancels the operation.</param>
    /// <returns>
    /// The content hash and stored size; re-uploading the same bytes returns the
    /// same hash and keeps the existing metadata.
    /// </returns>
    private static async Task<IResult> UploadArtifact(
        UploadArtifactRequest request,
        IArtifactStore store,
        IArtifactRepository repository,
        CancellationToken cancellationToken)
    {
        byte[] content;
        try
        {
            content = Convert.FromBase64String(request.ContentBase64);
        }
        catch (FormatException)
        {
            return Results.BadRequest("ContentBase64 is not valid base64.");
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
