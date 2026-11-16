using Microsoft.AspNetCore.Server.Kestrel.Core;
using Npgsql;
using WasmGate.Control.Api;
using WasmGate.Control.Api.Endpoints;
using WasmGate.Control.Api.Endpoints.Admin;
using WasmGate.Control.Api.Grpc;
using WasmGate.Control.Api.Hosting;
using WasmGate.Control.Api.Infrastructure.Networking;
using WasmGate.Control.Api.Infrastructure.Security;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Releases;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Infrastructure.Agents;
using WasmGate.Control.Infrastructure.Repositories;
using WasmGate.Control.Infrastructure.Security;
using AdminJsonContext = WasmGate.Control.Api.Contracts.AdminJsonContext;

// Composition root of the control plane: options, logging, persistence, gRPC
// services and the admin HTTP API. Everything is registered as a singleton,
// because the repositories are stateless over one connection pool and the
// services only coordinate them.
//
// The content root is the application directory, not the working directory: the
// admin UI lives in wwwroot next to the binary and must be found regardless of
// where the service was started.
var builder = WebApplication.CreateSlimBuilder(new WebApplicationOptions
{
    Args = args,
    ContentRootPath = AppContext.BaseDirectory,
});

builder.SetupLogging();
builder.Services.ConfigureHttpJsonOptions(options =>
{
    options.SerializerOptions.TypeInfoResolverChain.Insert(0, AdminJsonContext.Default);
});

var options = IHostApplicationBuilder.GetCpOptions();
StartupSecurity.Validate(options);
builder.RegisterServices();
builder.WebHost.ConfigureKestrel(kestrel =>
{
    // Artifacts travel base64-encoded inside JSON, so the request body limit is
    // derived from the artifact limit.
    kestrel.Limits.MaxRequestBodySize = checked((options.MaxArtifactBytes * 4 / 3) + (1L << 20));
    ListenEndpoint.Add(kestrel, options.GrpcListen, HttpProtocols.Http2, options.GrpcTls);
    ListenEndpoint.Add(kestrel, options.HttpListen, HttpProtocols.Http1);
});

builder.Services.AddSingleton(options);
builder.Services.AddSingleton(_ => new NpgsqlDataSourceBuilder(options.PostgresConnectionString).Build());

builder.Services.AddSingleton<ICertificateAuthority>(new PkiCertificateAuthority(options));
builder.Services.AddSingleton<IEnrollmentTokenRepository, PostgresEnrollmentTokenRepository>();
builder.Services.AddSingleton<IAgentCertificateRepository, PostgresAgentCertificateRepository>();
builder.Services.AddSingleton<IAgentProvisioningService, AgentProvisioningService>();
builder.Services.AddSingleton<IReleaseService, ReleaseService>();

builder.Services.AddGrpc();
builder.Services.AddSingleton<AgentControlService>();
builder.Services.AddSingleton<AgentEnrollmentService>();
builder.Services.AddHostedService<DeploymentReaper>();

var app = builder.Build();

// The admin UI is a static SPA served from wwwroot. It uses the admin API on the
// same origin, so no CORS is involved; the API itself stays protected by the
// bearer token.
app.UseDefaultFiles();
app.UseStaticFiles();
app.UseAdminToken(options.AdminToken);

app.MapGrpcService<AgentControlService>();
app.MapGrpcService<AgentEnrollmentService>();
app.MapGrpcService<ArtifactDownloadService>();
app.MapAdminEndpoints();

app.Run();
