using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;
using Grpc.Core;
using Microsoft.Extensions.Logging.Abstractions;
using WasmGate.Control.Api.Grpc;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities.Agents;

namespace WasmGate.Control.Tests.Api;

/// <summary>
/// Context-based checks for rejected Session and Download calls. The full
/// Kestrel/mTLS flow is intentionally a follow-up; these tests exercise the
/// shared certificate gate without a network listener.
/// </summary>
public sealed class CertificateAuthenticationTests
{
    [Fact]
    public async Task ActiveCertificateIsAccepted()
    {
        var (certificate, agentId) = CreateCertificate();
        var service = NewControlService(new StubCertificateRepository(active: true));
        var download = NewDownloadService(new StubCertificateRepository(active: true));

        var thumbprint = await service.ValidateCertificateAsync(
            certificate,
            agentId,
            CancellationToken.None);
        await download.EnsureAuthorizedAsync(certificate, CancellationToken.None);

        Assert.Equal(AgentCertificateIdentity.Thumbprint(certificate), thumbprint);
    }

    [Fact]
    public async Task RevokedCertificateIsRejectedForSessionAndDownload()
    {
        var (certificate, agentId) = CreateCertificate();
        var service = NewControlService(new StubCertificateRepository(active: false));
        var download = NewDownloadService(new StubCertificateRepository(active: false));

        var sessionError = await Assert.ThrowsAsync<RpcException>(
            () => service.ValidateCertificateAsync(
                certificate,
                agentId,
                CancellationToken.None));
        var downloadError = await Assert.ThrowsAsync<RpcException>(
            () => download.EnsureAuthorizedAsync(certificate, CancellationToken.None));

        Assert.Equal(StatusCode.Unauthenticated, sessionError.StatusCode);
        Assert.Equal(StatusCode.Unauthenticated, downloadError.StatusCode);
    }

    [Fact]
    public async Task CertificateForAnotherAgentIsRejected()
    {
        var (certificate, _) = CreateCertificate();
        var service = NewControlService(new StubCertificateRepository(active: true));

        var error = await Assert.ThrowsAsync<RpcException>(
            () => service.ValidateCertificateAsync(
                certificate,
                Guid.NewGuid(),
                CancellationToken.None));

        Assert.Equal(StatusCode.PermissionDenied, error.StatusCode);
    }

    [Fact]
    public async Task MissingCertificateIsRejectedWhenMtlsIsRequired()
    {
        var service = NewControlService(new StubCertificateRepository(active: true));

        var error = await Assert.ThrowsAsync<RpcException>(
            () => service.ValidateCertificateAsync(
                certificate: null,
                Guid.NewGuid(),
                CancellationToken.None));

        Assert.Equal(StatusCode.Unauthenticated, error.StatusCode);
    }

    private static AgentControlService NewControlService(
        IAgentCertificateRepository certificates) => new(
        registry: null!,
        agents: null!,
        certificates: certificates,
        deployments: null!,
        audit: null!,
        options: MtlsOptions(),
        logger: NullLogger<AgentControlService>.Instance);

    private static ArtifactDownloadService NewDownloadService(
        IAgentCertificateRepository certificates) => new(
        artifacts: null!,
        certificates: certificates,
        options: MtlsOptions(),
        logger: NullLogger<ArtifactDownloadService>.Instance);

    private static ControlPlaneOptions MtlsOptions() => new()
    {
        GrpcTls = new ControlPlaneTlsOptions
        {
            CertificatePath = "server.crt",
            PrivateKeyPath = "server.key",
            RequireClientCertificate = true,
        },
    };

    private static (X509Certificate2 Certificate, Guid AgentId) CreateCertificate()
    {
        var agentId = Guid.NewGuid();
        using var key = RSA.Create(2048);
        var request = new CertificateRequest(
            $"CN=wasmgate-agent:{agentId:D}",
            key,
            HashAlgorithmName.SHA256,
            RSASignaturePadding.Pkcs1);
        using var issued = request.CreateSelfSigned(
            DateTimeOffset.UtcNow.AddMinutes(-1),
            DateTimeOffset.UtcNow.AddHours(1));
        return (
            X509CertificateLoader.LoadCertificate(issued.Export(X509ContentType.Cert)),
            agentId);
    }

    private sealed class StubCertificateRepository(bool active) : IAgentCertificateRepository
    {
        public Task AddAsync(AgentCertificate certificate, CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected AddAsync");

        public Task<IReadOnlyList<AgentCertificate>> ListActiveByAgentAsync(
            Guid agentId,
            CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected ListActiveByAgentAsync");

        public Task<bool> IsActiveAsync(
            Guid agentId,
            string thumbprint,
            CancellationToken cancellationToken) =>
            Task.FromResult(active);

        public Task<bool> RevokeAsync(
            Guid agentId,
            string serialNumber,
            CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected RevokeAsync");

        public Task<CertificateRotationResult> RotateAsync(
            AgentCertificate certificate,
            string? callerThumbprint,
            CancellationToken cancellationToken) =>
            throw new InvalidOperationException("unexpected RotateAsync");
    }
}
