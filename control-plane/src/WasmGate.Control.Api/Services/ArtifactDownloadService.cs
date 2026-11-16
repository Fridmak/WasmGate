using System.Security.Cryptography.X509Certificates;
using Google.Protobuf;
using Grpc.Core;
using WasmGate.Control.Core.Abstractions.Releases;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.V1;

namespace WasmGate.Control.Api.Services;

/// <summary>
/// Streams content-addressed release artifacts to enrolled data planes.
/// </summary>
/// <remarks>
/// The stream is protected by the same mTLS identity as
/// <c>AgentControl.Session</c>: when TLS requires a client certificate, the
/// caller must present one with a <c>CN=wasmgate-agent:&lt;guid&gt;</c> subject.
/// Artifact identifiers are SHA-256 hashes, so the agent can verify the bytes it
/// received.
/// </remarks>
public sealed class ArtifactDownloadService(
    IArtifactStore artifacts,
    ControlPlaneOptions options,
    ILogger<ArtifactDownloadService> logger) : AgentArtifacts.AgentArtifactsBase
{
    /// <summary>Chunk size used for one <see cref="ArtifactChunk"/> message.</summary>
    private const int ChunkSize = 64 * 1024;

    /// <summary>
    /// Streams one artifact to an enrolled agent.
    /// </summary>
    /// <param name="request">SHA-256 identifier of the artifact.</param>
    /// <param name="responseStream">Stream that receives the artifact in 64 KiB chunks.</param>
    /// <param name="context">Context of the gRPC call.</param>
    /// <exception cref="RpcException">
    /// The caller is not an enrolled agent, the identifier is not a SHA-256 hash,
    /// or the artifact does not exist.
    /// </exception>
    public override async Task Download(
        ArtifactDownloadRequest request,
        IServerStreamWriter<ArtifactChunk> responseStream,
        ServerCallContext context)
    {
        EnsureAuthorized(context);

        var artifactId = request.Id?.Trim() ?? string.Empty;
        if (!IsSha256(artifactId))
        {
            throw new RpcException(new Status(
                StatusCode.InvalidArgument,
                "Artifact id must be a 64-character SHA-256 hex string."));
        }

        if (!await artifacts.ExistsAsync(artifactId, context.CancellationToken))
        {
            throw new RpcException(new Status(
                StatusCode.NotFound,
                $"Artifact '{artifactId}' does not exist."));
        }

        await using var source = await artifacts.OpenReadAsync(artifactId, context.CancellationToken);
        var buffer = new byte[ChunkSize];
        long total = 0;

        int read;
        while ((read = await source.ReadAsync(buffer.AsMemory(), context.CancellationToken)) > 0)
        {
            await responseStream.WriteAsync(new ArtifactChunk
            {
                Data = ByteString.CopyFrom(buffer, 0, read),
            });
            total += read;
        }

        logger.LogDebug("artifact {ArtifactId} streamed to agent: {Bytes} bytes", artifactId, total);
    }

    /// <summary>Rejects callers that do not present an agent certificate when mTLS is required.</summary>
    /// <param name="context">Current gRPC call context.</param>
    /// <exception cref="RpcException">The caller has no valid agent certificate.</exception>
    private void EnsureAuthorized(ServerCallContext context)
    {
        if (options.GrpcTls.CertificatePath is null || !options.GrpcTls.RequireClientCertificate)
        {
            return;
        }

        var certificate = context.GetHttpContext()?.Connection.ClientCertificate;
        if (certificate is not null &&
            AgentNaming.TryParseCertificateCommonName(
                certificate.GetNameInfo(X509NameType.SimpleName, forIssuer: false),
                out _))
        {
            return;
        }

        throw new RpcException(new Status(
            StatusCode.Unauthenticated,
            "A client certificate with CN=wasmgate-agent:<guid> is required."));
    }

    /// <summary>Checks that a value is a lower-case-insensitive SHA-256 hex string.</summary>
    /// <param name="value">Artifact identifier from the request.</param>
    /// <returns>True when the value is 64 hexadecimal characters.</returns>
    private static bool IsSha256(string value) =>
        value.Length == 64 && value.All(Uri.IsHexDigit);
}
