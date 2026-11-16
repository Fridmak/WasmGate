using Microsoft.AspNetCore.Server.Kestrel.Core;
using Npgsql;
using WasmGate.Control.Api;
using WasmGate.Control.Api.Endpoints;
using WasmGate.Control.Api.Infrastructure.Networking;
using WasmGate.Control.Api.Services;
using WasmGate.Control.Core.Abstractions.Agents;
using WasmGate.Control.Core.Abstractions.Releases;
using WasmGate.Control.Core.Abstractions.Repositories;
using WasmGate.Control.Infrastructure.Agents;
using WasmGate.Control.Infrastructure.Repositories;
using WasmGate.Control.Infrastructure.Security;

// Composition root of the control plane: options, logging, persistence, gRPC
// services and the admin HTTP API. Everything is registered as a singleton,
// because the repositories are stateless over one connection pool and the
// services only coordinate them.
var builder = WebApplication.CreateSlimBuilder(args);

builder.SetupLogging();
builder.Services.ConfigureHttpJsonOptions(options =>
{
    options.SerializerOptions.TypeInfoResolverChain.Insert(0, AdminJsonContext.Default);
});

var options = IHostApplicationBuilder.GetCpOptions();
builder.RegisterServices();
builder.WebHost.ConfigureKestrel(kestrel =>
{
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

var app = builder.Build();

app.MapGrpcService<AgentControlService>();
app.MapGrpcService<AgentEnrollmentService>();
app.MapGrpcService<ArtifactDownloadService>();
app.MapAdminEndpoints();

app.Run();
