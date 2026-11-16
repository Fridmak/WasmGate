using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;
using Grpc.Core;
using WasmGate.Control.Core.Entities.Agents;

namespace WasmGate.Control.Api.Grpc;

/// <summary>
/// Reads the agent identity from the client certificate presented to a gRPC
/// call.
/// </summary>
/// <remarks>
/// The CN format is owned by <see cref="AgentNaming"/>; this helper only bridges
/// it to <see cref="ServerCallContext"/>. The TLS layer validates the chain, so
/// here we only need the identity and the fingerprint.
/// </remarks>
internal static class AgentCertificateIdentity
{
    /// <summary>Returns the client certificate of the call, if one was presented.</summary>
    /// <param name="context">Current gRPC call context.</param>
    public static X509Certificate2? GetCertificate(ServerCallContext context) =>
        context.GetHttpContext()?.Connection.ClientCertificate;

    /// <summary>Extracts the agent identifier from the certificate subject CN.</summary>
    /// <param name="context">Current gRPC call context.</param>
    /// <param name="agentId">Agent identifier encoded in the certificate.</param>
    /// <returns>True when a certificate with a <c>wasmgate-agent:&lt;guid&gt;</c> CN is present.</returns>
    public static bool TryGetAgentId(ServerCallContext context, out Guid agentId)
    {
        agentId = Guid.Empty;
        var certificate = GetCertificate(context);
        return certificate is not null &&
               AgentNaming.TryParseCertificateCommonName(
                   certificate.GetNameInfo(X509NameType.SimpleName, forIssuer: false),
                   out agentId);
    }

    /// <summary>Computes the fingerprint the control plane stores for a certificate.</summary>
    /// <param name="certificate">Certificate presented by the agent, or null.</param>
    /// <returns>Upper-case hex SHA-256 of the DER encoding, or null.</returns>
    public static string? Thumbprint(X509Certificate2? certificate) =>
        certificate is null ? null : Convert.ToHexString(SHA256.HashData(certificate.RawData));
}
