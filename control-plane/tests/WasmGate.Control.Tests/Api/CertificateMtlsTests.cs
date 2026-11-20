using System.Net;
using System.Net.Sockets;
using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;
using Google.Protobuf;
using Grpc.Core;
using Grpc.Net.Client;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Server.Kestrel.Core;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Logging.Abstractions;
using WasmGate.Control.Api;
using WasmGate.Control.Api.Endpoints.Agents;
using WasmGate.Control.Api.Grpc;
using WasmGate.Control.Api.Infrastructure.Networking;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Releases;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Core.Entities.Agents;
using WasmGate.Control.Infrastructure.Agents;
using WasmGate.Control.Infrastructure.Storage;
using WasmGate.Control.V1;

namespace WasmGate.Control.Tests.Api;

/// <summary>
/// End-to-end certificate authentication over a real Kestrel HTTPS listener.
/// No TestServer and no fake ServerCallContext: the client certificate is
/// validated by Kestrel, then by the control-plane certificate gate.
/// </summary>
public sealed class CertificateMtlsTests
{
    private static readonly TimeSpan Timeout = TimeSpan.FromSeconds(10);

    [Fact]
    public async Task RevokedClientCertificateCannotOpenARealMtlsSession()
    {
        var material = TestCertificateMaterial.Create();
        WebApplication? app = null;
        try
        {
            var options = CreateOptions(material);
            var grpcPort = new Uri(options.GrpcListen).Port;
            app = BuildHost(options);
            var agentId = material.AgentId;
            var agent = new Agent
            {
                Id = agentId,
                Name = "mtls-agent",
                Environment = "prod",
                Version = "test",
                Status = AgentStatus.Pending,
                RegisteredAt = DateTimeOffset.UtcNow,
                LastSeenAt = DateTimeOffset.UtcNow,
            };

            await app.Services.GetRequiredService<SqliteStorageInitializer>()
                .InitializeAsync(CancellationToken.None);
            await app.Services.GetRequiredService<IAgentRepository>()
                .UpsertAsync(agent, CancellationToken.None);
            var certificates = app.Services.GetRequiredService<IAgentCertificateRepository>();
            await certificates.AddAsync(new AgentCertificate
            {
                AgentId = agentId,
                SerialNumber = "mtls-serial",
                Thumbprint = AgentCertificateIdentity.Thumbprint(material.ClientCertificate)!,
                Pem = material.ClientCertificate.ExportCertificatePem(),
                NotBefore = material.ClientCertificate.NotBefore,
                NotAfter = material.ClientCertificate.NotAfter,
            }, CancellationToken.None);
            _ = app.Services.GetRequiredService<IReleaseSigner>();

            app.MapGrpcService<AgentControlService>();
            await app.StartAsync();

            using var handler = new HttpClientHandler
            {
                ClientCertificateOptions = ClientCertificateOption.Manual,
                ServerCertificateCustomValidationCallback = (_, _, _, _) => true,
            };
            handler.ClientCertificates.Add(material.ClientCertificate);
            using var channel = GrpcChannel.ForAddress(
                $"https://127.0.0.1:{grpcPort}",
                new GrpcChannelOptions { HttpHandler = handler });
            var invoker = channel.CreateCallInvoker();
            var registry = app.Services.GetRequiredService<IAgentSessionRegistry>();

            using (var call = invoker.AsyncDuplexStreamingCall(
                       SessionMethod,
                       host: null,
                       new CallOptions()))
            {
                await call.RequestStream.WriteAsync(Hello(agentId));
                await WaitUntilAsync(
                    () => registry.TryGet(agentId, out _),
                    "the mTLS session was not registered");

                var revoke = await AgentEndpoints.RevokeCertificate(
                    agentId,
                    "mtls-serial",
                    certificates,
                    registry,
                    app.Services.GetRequiredService<IAgentOperationLock>(),
                    app.Services.GetRequiredService<IAuditLog>(),
                    NullLogger<Program>.Instance,
                    CancellationToken.None);
                Assert.Equal(StatusCodes.Status204NoContent, ((IStatusCodeHttpResult)revoke).StatusCode);
                await WaitUntilAsync(
                    () => !registry.TryGet(agentId, out _),
                    "the revoked session was not removed");

                var move = call.ResponseStream.MoveNext(CancellationToken.None);
                var completed = await Task.WhenAny(move, Task.Delay(Timeout));
                Assert.Same(move, completed);
                try
                {
                    await move;
                }
                catch (RpcException)
                {
                    // The server may surface cancellation as an RPC status.
                }
            }

            using var rejected = invoker.AsyncDuplexStreamingCall(
                SessionMethod,
                host: null,
                new CallOptions());
            RpcException? failure = null;
            try
            {
                await rejected.RequestStream.WriteAsync(Hello(agentId));
            }
            catch (RpcException ex)
            {
                failure = ex;
            }

            failure ??= await Assert.ThrowsAsync<RpcException>(
                () => rejected.ResponseStream.MoveNext(CancellationToken.None));
            Assert.Equal(StatusCode.Unauthenticated, failure.StatusCode);
        }
        finally
        {
            if (app is not null)
            {
                try
                {
                    await app.StopAsync();
                }
                catch (Exception)
                {
                    // Preserve the original test failure.
                }

                await app.DisposeAsync();
            }

            material.Dispose();
        }
    }

    private static readonly Method<AgentMessage, ControlMessage> SessionMethod = new(
        MethodType.DuplexStreaming,
        "wasmgate.control.v1.AgentControl",
        "Session",
        Marshallers.Create(
            (AgentMessage message) => message.ToByteArray(),
            (byte[] data) => AgentMessage.Parser.ParseFrom(data)),
        Marshallers.Create(
            (ControlMessage message) => message.ToByteArray(),
            (byte[] data) => ControlMessage.Parser.ParseFrom(data)));

    private static AgentMessage Hello(Guid agentId) => new()
    {
        Hello = new Hello
        {
            AgentId = agentId.ToString("D"),
            Name = "mtls-agent",
            Version = "test",
        },
    };

    private static ControlPlaneOptions CreateOptions(TestCertificateMaterial material)
    {
        var port = FreePort();
        return new ControlPlaneOptions
        {
            Local = false,
            SqlitePath = Path.Combine(material.Directory, "control.db"),
            GrpcListen = $"https://127.0.0.1:{port}",
            HttpListen = $"http://127.0.0.1:{FreePort()}",
            ArtifactRoot = Path.Combine(material.Directory, "artifacts"),
            SigningKeyPath = Path.Combine(material.Directory, "keys", "release-signing.key"),
            AgentCaCertPath = Path.Combine(material.Directory, "ca", "agents-ca.crt"),
            AgentCaKeyPath = Path.Combine(material.Directory, "ca", "agents-ca.key"),
            AdminToken = "mtls-test-token",
            GrpcTls = new ControlPlaneTlsOptions
            {
                CertificatePath = material.ServerCertificatePath,
                PrivateKeyPath = material.ServerKeyPath,
                ClientCaPath = material.ClientCaPath,
                RequireClientCertificate = true,
            },
        };
    }

    private static WebApplication BuildHost(ControlPlaneOptions options)
    {
        var builder = WebApplication.CreateSlimBuilder(new WebApplicationOptions
        {
            ContentRootPath = AppContext.BaseDirectory,
            EnvironmentName = "Production",
        });
        builder.WebHost.ConfigureKestrel(kestrel =>
            ListenEndpoint.Add(kestrel, options.GrpcListen, HttpProtocols.Http2, options.GrpcTls));
        builder.Services.AddSingleton(options);
        builder.RegisterServices(options);
        builder.Services.AddGrpc();
        builder.Services.AddSingleton<AgentControlService>();
        return builder.Build();
    }

    private static async Task WaitUntilAsync(Func<bool> condition, string message)
    {
        var deadline = DateTimeOffset.UtcNow + Timeout;
        while (!condition())
        {
            if (DateTimeOffset.UtcNow >= deadline)
            {
                Assert.Fail(message);
            }

            await Task.Delay(10);
        }
    }

    private static int FreePort()
    {
        using var listener = new TcpListener(IPAddress.Loopback, 0);
        listener.Start();
        var port = ((IPEndPoint)listener.LocalEndpoint).Port;
        listener.Stop();
        return port;
    }

    private sealed class TestCertificateMaterial : IDisposable
    {
        private readonly HashSet<string> _existingKeyContainers;
        private readonly DateTime _createdAtUtc;

        private TestCertificateMaterial(
            string directory,
            string serverCertificatePath,
            string serverKeyPath,
            string clientCaPath,
            Guid agentId,
            X509Certificate2 clientCertificate,
            HashSet<string> existingKeyContainers,
            DateTime createdAtUtc)
        {
            Directory = directory;
            ServerCertificatePath = serverCertificatePath;
            ServerKeyPath = serverKeyPath;
            ClientCaPath = clientCaPath;
            AgentId = agentId;
            ClientCertificate = clientCertificate;
            _existingKeyContainers = existingKeyContainers;
            _createdAtUtc = createdAtUtc;
        }

        public string Directory { get; }

        public string ServerCertificatePath { get; }

        public string ServerKeyPath { get; }

        public string ClientCaPath { get; }

        public Guid AgentId { get; }

        public X509Certificate2 ClientCertificate { get; }

        public static TestCertificateMaterial Create()
        {
            var directory = Path.Combine(
                Path.GetTempPath(),
                "wasmgate-mtls-tests",
                Guid.NewGuid().ToString("N"));
            System.IO.Directory.CreateDirectory(directory);
            var agentId = Guid.NewGuid();
            var createdAtUtc = DateTime.UtcNow;
            var existingKeyContainers = SnapshotKeyContainers();

            // One time base for every certificate: calling UtcNow separately can
            // give an issued certificate a notAfter a tick beyond its CA and make
            // CertificateRequest.Create reject it.
            var notBefore = DateTimeOffset.UtcNow.AddMinutes(-5);
            var notAfter = notBefore.AddHours(1);

            using var serverKey = RSA.Create(2048);
            var serverRequest = new CertificateRequest(
                "CN=localhost",
                serverKey,
                HashAlgorithmName.SHA256,
                RSASignaturePadding.Pkcs1);
            var san = new SubjectAlternativeNameBuilder();
            san.AddDnsName("localhost");
            san.AddIpAddress(IPAddress.Loopback);
            serverRequest.CertificateExtensions.Add(san.Build());
            serverRequest.CertificateExtensions.Add(
                new X509BasicConstraintsExtension(false, false, 0, false));
            serverRequest.CertificateExtensions.Add(
                new X509KeyUsageExtension(
                    X509KeyUsageFlags.DigitalSignature | X509KeyUsageFlags.KeyEncipherment,
                    critical: true));
            serverRequest.CertificateExtensions.Add(
                new X509EnhancedKeyUsageExtension(
                    new OidCollection { new("1.3.6.1.5.5.7.3.1") },
                    critical: false));
            using var serverCertificate = serverRequest.CreateSelfSigned(
                notBefore,
                notAfter);
            var serverCertificatePath = Path.Combine(directory, "server.crt");
            var serverKeyPath = Path.Combine(directory, "server.key");
            File.WriteAllText(serverCertificatePath, serverCertificate.ExportCertificatePem());
            File.WriteAllText(serverKeyPath, serverKey.ExportPkcs8PrivateKeyPem());

            using var caKey = RSA.Create(2048);
            var caRequest = new CertificateRequest(
                "CN=WasmGate Test Client CA",
                caKey,
                HashAlgorithmName.SHA256,
                RSASignaturePadding.Pkcs1);
            caRequest.CertificateExtensions.Add(
                new X509BasicConstraintsExtension(true, false, 0, true));
            caRequest.CertificateExtensions.Add(
                new X509KeyUsageExtension(
                    X509KeyUsageFlags.KeyCertSign | X509KeyUsageFlags.CrlSign,
                    critical: true));
            using var caCertificate = caRequest.CreateSelfSigned(
                notBefore,
                notAfter);
            var clientCaPath = Path.Combine(directory, "client-ca.crt");
            File.WriteAllText(clientCaPath, caCertificate.ExportCertificatePem());

            using var clientKey = RSA.Create(2048);
            var clientRequest = new CertificateRequest(
                $"CN=wasmgate-agent:{agentId:D}",
                clientKey,
                HashAlgorithmName.SHA256,
                RSASignaturePadding.Pkcs1);
            clientRequest.CertificateExtensions.Add(
                new X509BasicConstraintsExtension(false, false, 0, false));
            clientRequest.CertificateExtensions.Add(
                new X509KeyUsageExtension(X509KeyUsageFlags.DigitalSignature, critical: true));
            clientRequest.CertificateExtensions.Add(
                new X509EnhancedKeyUsageExtension(
                    new OidCollection { new("1.3.6.1.5.5.7.3.2") },
                    critical: false));
            using var issued = clientRequest.Create(
                caCertificate,
                notBefore,
                notAfter,
                RandomNumberGenerator.GetBytes(16));
            using var withKey = issued.CopyWithPrivateKey(clientKey);
            // Schannel on Windows needs a usable persisted private key for a
            // client certificate; EphemeralKeySet is rejected by AcquireCredentialsHandle.
            var keyStorageFlags = OperatingSystem.IsWindows()
                ? X509KeyStorageFlags.UserKeySet
                : X509KeyStorageFlags.EphemeralKeySet;
            var clientCertificate = X509CertificateLoader.LoadPkcs12(
                withKey.Export(X509ContentType.Pfx),
                password: null,
                keyStorageFlags);

            return new TestCertificateMaterial(
                directory,
                serverCertificatePath,
                serverKeyPath,
                clientCaPath,
                agentId,
                clientCertificate,
                existingKeyContainers,
                createdAtUtc);
        }

        public void Dispose()
        {
            ClientCertificate.Dispose();
            DeleteNewKeyContainers(_existingKeyContainers, _createdAtUtc);
            try
            {
                System.IO.Directory.Delete(Directory, recursive: true);
            }
            catch (IOException)
            {
                // Do not hide an assertion failure behind temp cleanup.
            }
        }

        // Schannel needs a persisted private key for TLS certificates on
        // Windows, so a run leaves key containers in the user profile. Remove
        // only the containers that this run created.
        private static string KeyContainerDirectory => Path.Combine(
            Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData),
            "Microsoft",
            "Crypto",
            "Keys");

        private static HashSet<string> SnapshotKeyContainers()
        {
            var directory = KeyContainerDirectory;
            if (!System.IO.Directory.Exists(directory))
            {
                return new HashSet<string>(StringComparer.OrdinalIgnoreCase);
            }

            return System.IO.Directory
                .EnumerateFiles(directory)
                .Select(Path.GetFileName)
                .OfType<string>()
                .ToHashSet(StringComparer.OrdinalIgnoreCase);
        }

        private static void DeleteNewKeyContainers(
            HashSet<string> existing,
            DateTime createdAtUtc)
        {
            if (!OperatingSystem.IsWindows())
            {
                return;
            }

            var directory = KeyContainerDirectory;
            if (!System.IO.Directory.Exists(directory))
            {
                return;
            }

            foreach (var path in System.IO.Directory.EnumerateFiles(directory))
            {
                var name = Path.GetFileName(path);
                if (name is null
                    || existing.Contains(name)
                    || File.GetLastWriteTimeUtc(path) < createdAtUtc)
                {
                    continue;
                }

                try
                {
                    File.Delete(path);
                }
                catch (IOException)
                {
                    // A key still in use is reclaimed by the OS later.
                }
                catch (UnauthorizedAccessException)
                {
                    // Best effort: never fail a test because of profile cleanup.
                }
            }
        }
    }
}
