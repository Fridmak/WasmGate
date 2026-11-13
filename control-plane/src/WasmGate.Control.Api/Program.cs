using System.Text.Json;
using Microsoft.AspNetCore.Server.Kestrel.Core;
using Npgsql;
using WasmGate.Control.Api;
using WasmGate.Control.Api.Endpoints;
using WasmGate.Control.Api.Infrastructure;
using WasmGate.Control.Api.Services;
using WasmGate.Control.Core.Abstractions;
using WasmGate.Control.Core.Configuration;
using WasmGate.Control.Infrastructure.Agents;
using WasmGate.Control.Infrastructure.Artifacts;
using WasmGate.Control.Infrastructure.Postgres;
using WasmGate.Control.Infrastructure.Security;
using WasmGate.Control.Infrastructure.Validation;

var builder = WebApplication.CreateSlimBuilder(args);

builder.SetupLogging();
builder.Services.ConfigureHttpJsonOptions(options =>
{
    options.SerializerOptions.TypeInfoResolverChain.Insert(0, AdminJsonContext.Default);
});

var options = builder.GetCPOptions();
builder.RegisterServices();
builder.WebHost.ConfigureKestrel(kestrel =>
{
    ListenEndpoint.Add(kestrel, options.GrpcListen, HttpProtocols.Http2, options.GrpcTls);
    ListenEndpoint.Add(kestrel, options.HttpListen, HttpProtocols.Http1);
});

builder.Services.AddSingleton(options);
builder.Services.AddSingleton(_ => new NpgsqlDataSourceBuilder(options.PostgresConnectionString).Build());

builder.Services.AddSingleton<ICertificateAuthority>(new PkiCertificateAuthority(options));
builder.Services.AddSingleton<IEnrollmentTokenRepository, InMemoryEnrollmentTokenRepository>();
builder.Services.AddSingleton<IAgentCertificateRepository, InMemoryAgentCertificateRepository>();
builder.Services.AddSingleton<IAgentProvisioningService, AgentProvisioningService>();
builder.Services.AddSingleton<IReleaseService, ReleaseService>();

builder.Services.AddGrpc();
builder.Services.AddSingleton<AgentControlService>();
builder.Services.AddSingleton<AgentEnrollmentService>();

var app = builder.Build();

app.MapGrpcService<AgentControlService>();
app.MapGrpcService<AgentEnrollmentService>();
app.MapAdminEndpoints();

app.Run();
