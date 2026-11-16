using Google.Protobuf;
using Grpc.Core;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Entities.Agents.Enrollment;
using WasmGate.Control.V1;

namespace WasmGate.Control.Api.Grpc;

/// <summary>
/// Issues and renews the mTLS certificate of a data plane.
/// </summary>
/// <remarks>
/// <c>Enroll</c> is unauthenticated by design: the agent has no certificate yet,
/// and an enrollment token authorises it. <c>Renew</c> is the opposite: it
/// requires an existing, non-revoked certificate, so a leaked token is useless
/// for renewal and a stolen certificate stops working once it is revoked.
/// Failure kinds are mapped to gRPC status codes, so the data plane can tell a
/// bad token from a name conflict.
/// </remarks>
public sealed class AgentEnrollmentService(
    IAgentProvisioningService provisioning,
    IAgentCertificateRepository certificates,
    ILogger<AgentEnrollmentService> logger) : AgentEnrollment.AgentEnrollmentBase
{
    /// <summary>
    /// Issues a client certificate in exchange for an enrollment token.
    /// </summary>
    /// <param name="request">Agent identifier, token, name and CSR.</param>
    /// <param name="context">Context of the gRPC call.</param>
    /// <returns>The issued certificate, the agent CA and the expiry.</returns>
    public override async Task<EnrollResponse> Enroll(
        EnrollRequest request,
        ServerCallContext context)
    {
        try
        {
            var result = await provisioning.EnrollAsync(
                request.AgentId,
                request.Name,
                request.Token,
                request.CsrPem.ToStringUtf8(),
                context.CancellationToken);

            logger.LogInformation(
                "agent enrolled: {AgentId} name={Name} expires={ExpiresAt}",
                request.AgentId,
                request.Name,
                result.NotAfter);

            return ToResponse(result);
        }
        catch (EnrollmentException ex)
        {
            throw ToRpcException(ex, request.AgentId, request.Name);
        }
    }

    /// <summary>
    /// Replaces the caller's certificate before it expires.
    /// </summary>
    /// <param name="request">Agent identifier and CSR for the new key.</param>
    /// <param name="context">Context of the gRPC call.</param>
    /// <returns>The issued certificate, the agent CA and the new expiry.</returns>
    /// <exception cref="RpcException">
    /// The call has no valid agent certificate, it names another agent, or it has
    /// been revoked.
    /// </exception>
    public override async Task<EnrollResponse> Renew(
        RenewRequest request,
        ServerCallContext context)
    {
        if (!AgentCertificateIdentity.TryGetAgentId(context, out var certificateAgentId))
        {
            throw new RpcException(new Status(
                StatusCode.Unauthenticated,
                "Renewal requires the agent client certificate."));
        }

        if (!Guid.TryParse(request.AgentId, out var requestedAgentId) ||
            requestedAgentId != certificateAgentId)
        {
            throw new RpcException(new Status(
                StatusCode.PermissionDenied,
                "The client certificate does not match the requested agent id."));
        }

        var thumbprint = AgentCertificateIdentity.Thumbprint(
            AgentCertificateIdentity.GetCertificate(context))!;
        if (!await certificates.IsActiveAsync(
                requestedAgentId,
                thumbprint,
                context.CancellationToken))
        {
            throw new RpcException(new Status(
                StatusCode.Unauthenticated,
                "The client certificate was revoked or has expired."));
        }

        try
        {
            var result = await provisioning.RenewAsync(
                requestedAgentId,
                request.CsrPem.ToStringUtf8(),
                context.CancellationToken);

            logger.LogInformation(
                "agent renewed: {AgentId} expires={ExpiresAt}",
                requestedAgentId,
                result.NotAfter);

            return ToResponse(result);
        }
        catch (EnrollmentException ex)
        {
            throw ToRpcException(ex, request.AgentId, name: null);
        }
    }

    /// <summary>Maps an application result to the wire response.</summary>
    /// <param name="result">Issued certificate and its metadata.</param>
    private static EnrollResponse ToResponse(EnrollmentResult result) => new()
    {
        CertificatePem = ByteString.CopyFromUtf8(result.CertificatePem),
        CaPem = ByteString.CopyFromUtf8(result.CaPem),
        ExpiresAtUnixMs = result.NotAfter.ToUnixTimeMilliseconds(),
    };

    /// <summary>Maps a provisioning failure to a gRPC status.</summary>
    /// <param name="exception">Failure raised by the provisioning service.</param>
    /// <param name="agentId">Agent id from the request, for the log entry.</param>
    /// <param name="name">Agent name from the request, or null for renewal.</param>
    private RpcException ToRpcException(EnrollmentException exception, string agentId, string? name)
    {
        var status = exception.Failure switch
        {
            EnrollmentFailure.CsrInvalid => StatusCode.InvalidArgument,
            EnrollmentFailure.InvalidRequest => StatusCode.InvalidArgument,
            EnrollmentFailure.AlreadyExists => StatusCode.AlreadyExists,
            EnrollmentFailure.InvalidToken => StatusCode.Unauthenticated,
            EnrollmentFailure.Expired => StatusCode.PermissionDenied,
            EnrollmentFailure.Exhausted => StatusCode.PermissionDenied,
            EnrollmentFailure.AgentMismatch => StatusCode.PermissionDenied,
            _ => StatusCode.Internal,
        };

        logger.LogWarning(
            "enrollment rejected: {Failure} agent={AgentId} name={Name}",
            exception.Failure,
            agentId,
            name);

        return new RpcException(new Status(status, exception.Message));
    }
}
