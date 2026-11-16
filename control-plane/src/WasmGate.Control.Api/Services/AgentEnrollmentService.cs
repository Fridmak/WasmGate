using Google.Protobuf;
using Grpc.Core;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Entities.Agents.Enrollment;
using WasmGate.Control.V1;

namespace WasmGate.Control.Api.Services;

/// <summary>
/// Issues the first mTLS certificate to a data plane.
/// </summary>
/// <remarks>
/// The call is unauthenticated by design: the agent has no certificate yet, and
/// the enrollment token authorises it. A single-use token is bound to one agent;
/// a registration token may be shared and optionally scoped by a name prefix.
/// Failure kinds are mapped to gRPC status codes, so the data plane can tell a
/// bad token from a name conflict.
/// </remarks>
public sealed class AgentEnrollmentService(
    IAgentProvisioningService provisioning,
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

            return new EnrollResponse
            {
                CertificatePem = ByteString.CopyFromUtf8(result.CertificatePem),
                CaPem = ByteString.CopyFromUtf8(result.CaPem),
                ExpiresAtUnixMs = result.NotAfter.ToUnixTimeMilliseconds(),
            };
        }
        catch (EnrollmentException ex)
        {
            var status = ex.Failure switch
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
                ex.Failure,
                request.AgentId,
                request.Name);
            throw new RpcException(new Status(status, ex.Message));
        }
    }
}
