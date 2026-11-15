using Google.Protobuf;
using Grpc.Core;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Entities;
using WasmGate.Control.V1;

namespace WasmGate.Control.Api.Services;

/// <summary>
/// Issues the first mTLS certificate to a data plane. Protected by a one-time
/// enrollment token; the agent does not have a client certificate at this point.
/// </summary>
public sealed class AgentEnrollmentService(
    IAgentProvisioningService provisioning,
    ILogger<AgentEnrollmentService> logger) : AgentEnrollment.AgentEnrollmentBase
{
    /// <inheritdoc />
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
